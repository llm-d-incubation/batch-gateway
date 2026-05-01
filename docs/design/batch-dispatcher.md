# Batch Dispatcher

Related:
- [\[Public Doc\] Serving Online Batch via Inference Gateway](https://docs.google.com/document/d/1notkq9s0qOmWmUNonZ8CfI-5jtGtHA4PGMI-xz8sGRE/edit?tab=t.0#heading=h.i76kzr3j3swj)
- [\[PUBLIC\] EPP Flow Controller for Priority, Fairness, and Queuing](https://docs.google.com/document/d/1VZL7opFWuwgWquvgiOzLlXAJ633qZ9U-A0ZixGjBgaI/edit?tab=t.0#heading=h.hfyow92z2d0t)
- [\[PUBLIC\] Improved Flow Control Request Management](https://docs.google.com/document/d/1JxzJc8gNv2wKK5-a8ohb0btn78ymVKw9XMIb4-S-ncA/edit?tab=t.0#heading=h.rutawybt03nl)
- [https://gateway-api-inference-extension.sigs.k8s.io/api-types/inferencepool/](https://gateway-api-inference-extension.sigs.k8s.io/api-types/inferencepool/)
- [Async Processor: Dispatch Gates](https://github.com/llm-d-incubation/llm-d-async/blob/main/README.md#dispatch-gates) (llm-d-async)
- [Dispatch Budget](https://github.com/llm-d-incubation/llm-d-async/blob/main/docs/dispatch-budget.md) (llm-d-async)

## Summary

This document details the design of a **Batch Dispatcher** (sometimes also referred to as the [**Async Processor**](https://github.com/llm-d-incubation/llm-d-async)) to extend the existing "online batch processing agent" architecture (see [\[Public Doc\] Serving Online Batch via Inference Gateway](https://docs.google.com/document/d/1notkq9s0qOmWmUNonZ8CfI-5jtGtHA4PGMI-xz8sGRE/edit?tab=t.0#heading=h.i76kzr3j3swj)). While the **llm-d Router** acts as the primary component for scheduling and flow control, the Batch Dispatcher serves as a system-load aware gatekeeper. It ensures that batch workloads (low-priority and sheddable) are pulled from message queues and forwarded to the llm-d Router only when the inference pool has sufficient capacity. This prevents low-priority traffic from flooding the system and competing with realtime requests.

## Problem statement

The current llm-d Router provides flow control, but a naive approach to batch processing without considering system limits can lead to competing for resources with higher-priority, interactive requests. Batch requests should not be blindly forwarded or retried without a mechanism to honor saturation thresholds; batch workloads may cause inefficient resource utilization or interfere with realtime traffic.

## Guiding Principles and Objectives

* **Reactive Flow Control:** Implement a best-effort, partially proactive mechanism to protect the system from unexpected overloads.
* **Decoupled Architecture:** Keep the llm-d Router as an independent, shared service while the Batch Dispatcher manages the "push" rate of sheddable workloads.

#### **Goals**

* Prevent online batch components from flooding the system, disrupting interactive traffic.
* Use existing and future metrics (e.g., "Dispatch Budget") to manage dispatching.
* Ensure batch workloads are configured with sheddable objectives to share capacity with realtime traffic.

#### **Non-Goals**

* **Fully Proactive Scheduling:** The scheduler remains mostly reactive.
* **Multi-tenancy/Policy Management:** Detailed tenant discrimination or complex priority queue policies are currently out of scope.

## Proposal

![](diagrams/batch-dispatcher.png)

The Batch Dispatcher sits between the message queue and the L7 Proxy. It may be thought of as an extension to the Batch Processing Agent in [\[Public Doc\] Serving Online Batch via Inference Gateway](https://docs.google.com/document/d/1notkq9s0qOmWmUNonZ8CfI-5jtGtHA4PGMI-xz8sGRE/edit?tab=t.0#heading=h.i76kzr3j3swj)

Because there is one InferencePool and one EPP per (base) model (see [InferencePool \- Kubernetes Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/api-types/inferencepool/)), there should be **one Batch Dispatcher per InferencePool**.

#### **Key Components**

* **Batch Processing Agent:** The component described in [\[Public Doc\] Serving Online Batch via Inference Gateway](https://docs.google.com/document/d/1notkq9s0qOmWmUNonZ8CfI-5jtGtHA4PGMI-xz8sGRE/edit?tab=t.0#heading=h.i76kzr3j3swj)
* **Batch Dispatcher:** A component that reads flow-control metrics, determines a "Dispatch Budget", and forwards sheddable traffic to the llm-d Router
* **Message Queue:** A persistent store (e.g., Redis, Pub/Sub, Kafka) that holds asynchronous requests. The queue is a priority queue, sorted according to some policy (e.g. an SLO, tenancy, etc: out of scope here)
* **Metrics Store:** Provides real-time data on **Inference Pool usage**. Such data drives the **Batch Dispatcher** logic
* **llm-d Router:** L7 Proxy \+ Endpoint Picker (EPP) \+ any other accessory service, it handles the final routing to model servers.

## Deployment Model and Lifecycle

The dispatcher corresponds to the **batch processing agent** described [\[Public Doc\] Serving Online Batch via Inference Gateway](https://docs.google.com/document/d/1notkq9s0qOmWmUNonZ8CfI-5jtGtHA4PGMI-xz8sGRE/edit?tab=t.0#heading=h.i76kzr3j3swj), effectively implementing a "strategy" to pull and forward requests from Message Queue to the llm-d Router.

Currently, the dispatcher is meant to be deployed stand-alone, with its own lifecycle. The dispatcher
can read from multiple queues and dispatch to multiple inference pools. It can be also configured to dispatch
to a single pool; in this case, you would configure one dispatcher for each pool.

## Open Questions & Thinking Points

* **Queue Granularity:** Should there be one queue per **InferencePool** or per model to avoid bottlenecks?

## Potential Improvements

* **Improved Metrics:** Develop heuristics to take into account the actual size/weight of the single request against the system capacity (currently we are only using the number of requests)
* **Active EPP:** Is it worth moving the pull logic directly into the EPP? This would require non-trivial changes to the internals so that a side channel is allowed to enqueue items on the internal HTTP request queues and to publish the results somewhere once they have been served.
* **Metric Latency:** Prometheus scraping adds delay. Should we implement an in-memory shared store between the EPP and Batch Dispatcher for faster updates? Should we instead scrape the metrics directly from the EPP/Inference Servers?
* **Active Notifications:** wake-up the scheduler instead of polling
