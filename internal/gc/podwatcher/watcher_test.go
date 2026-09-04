/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package podwatcher

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

const (
	testNamespace = "default"
	testStsName   = "processor"
	testSelector  = "app.kubernetes.io/component=processor"
)

func readyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{"app.kubernetes.io/component": "processor"},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func testStatefulSet(replicas int32, readyReplicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testStsName,
			Namespace: testNamespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas),
		},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas: readyReplicas,
		},
	}
}

func waitForLive(t *testing.T, ch <-chan map[string]bool) map[string]bool {
	t.Helper()
	select {
	case live := <-ch:
		return live
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for handler call")
		return nil
	}
}

func TestWatcher(t *testing.T) {
	t.Run("stable StatefulSet calls handler with ready pods", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			testStatefulSet(2, 2),
			readyPod("processor-0"),
			readyPod("processor-1"),
		)

		liveCh := make(chan map[string]bool, 10)
		w := &Watcher{
			clientset:        cs,
			namespace:        testNamespace,
			stsName:          testStsName,
			podLabelSelector: testSelector,
			handler:          func(live map[string]bool) { liveCh <- live },
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() { _ = w.Run(ctx) }()

		live := waitForLive(t, liveCh)
		if len(live) != 2 {
			t.Fatalf("expected 2 live pods, got %d: %v", len(live), live)
		}
		if !live["processor-0"] || !live["processor-1"] {
			t.Fatalf("expected processor-0 and processor-1, got %v", live)
		}
	})

	t.Run("unstable StatefulSet does not call handler", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			testStatefulSet(3, 2), // 3 desired, only 2 ready
			readyPod("processor-0"),
			readyPod("processor-1"),
		)

		liveCh := make(chan map[string]bool, 10)
		w := &Watcher{
			clientset:        cs,
			namespace:        testNamespace,
			stsName:          testStsName,
			podLabelSelector: testSelector,
			handler:          func(live map[string]bool) { liveCh <- live },
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() { _ = w.Run(ctx) }()

		select {
		case live := <-liveCh:
			t.Fatalf("handler should not be called when unstable, got %v", live)
		case <-time.After(500 * time.Millisecond):
			// Expected — handler not called.
		}
	})

	t.Run("scale up triggers handler with expanded set", func(t *testing.T) {
		sts := testStatefulSet(1, 1)
		cs := fake.NewSimpleClientset(
			sts,
			readyPod("processor-0"),
		)

		liveCh := make(chan map[string]bool, 10)
		w := &Watcher{
			clientset:        cs,
			namespace:        testNamespace,
			stsName:          testStsName,
			podLabelSelector: testSelector,
			handler:          func(live map[string]bool) { liveCh <- live },
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() { _ = w.Run(ctx) }()

		// Wait for initial stable state (1 replica).
		waitForLive(t, liveCh)

		// Simulate scale up: add a pod and update STS to 2 replicas.
		_, err := cs.CoreV1().Pods(testNamespace).Create(ctx, readyPod("processor-1"), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create pod: %v", err)
		}
		sts.Spec.Replicas = ptr.To(int32(2))
		sts.Status.ReadyReplicas = 2
		if _, err := cs.AppsV1().StatefulSets(testNamespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("failed to update sts: %v", err)
		}

		live := waitForLive(t, liveCh)
		if len(live) != 2 {
			t.Fatalf("expected 2 live pods after scale up, got %d: %v", len(live), live)
		}
		if !live["processor-0"] || !live["processor-1"] {
			t.Fatalf("expected processor-0 and processor-1, got %v", live)
		}
	})

	t.Run("scale down triggers handler with reduced set", func(t *testing.T) {
		sts := testStatefulSet(2, 2)
		cs := fake.NewSimpleClientset(
			sts,
			readyPod("processor-0"),
			readyPod("processor-1"),
		)

		liveCh := make(chan map[string]bool, 10)
		w := &Watcher{
			clientset:        cs,
			namespace:        testNamespace,
			stsName:          testStsName,
			podLabelSelector: testSelector,
			handler:          func(live map[string]bool) { liveCh <- live },
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() { _ = w.Run(ctx) }()

		// Wait for initial stable state.
		waitForLive(t, liveCh)

		// Simulate scale down: update STS to 1 replica and delete processor-1.
		sts.Spec.Replicas = ptr.To(int32(1))
		sts.Status.ReadyReplicas = 1
		_, err := cs.AppsV1().StatefulSets(testNamespace).Update(ctx, sts, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("failed to update sts: %v", err)
		}
		if err := cs.CoreV1().Pods(testNamespace).Delete(ctx, "processor-1", metav1.DeleteOptions{}); err != nil {
			t.Fatalf("failed to delete pod: %v", err)
		}

		live := waitForLive(t, liveCh)
		if len(live) != 1 {
			t.Fatalf("expected 1 live pod after scale down, got %d: %v", len(live), live)
		}
		if !live["processor-0"] {
			t.Fatalf("expected processor-0, got %v", live)
		}
	})
}

func TestIsPodReady(t *testing.T) {
	t.Run("ready pod", func(t *testing.T) {
		pod := readyPod("test")
		if !isPodReady(pod) {
			t.Error("expected pod to be ready")
		}
	})

	t.Run("not ready pod", func(t *testing.T) {
		pod := &corev1.Pod{
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				},
			},
		}
		if isPodReady(pod) {
			t.Error("expected pod to not be ready")
		}
	})

	t.Run("no conditions", func(t *testing.T) {
		pod := &corev1.Pod{}
		if isPodReady(pod) {
			t.Error("expected pod with no conditions to not be ready")
		}
	})
}
