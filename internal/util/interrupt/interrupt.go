// internal/util/interrupt/interrupt.go

package interrupt

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"
)

// ContextWithSignal monitors os signal, and return the context that cancels further works.
// Immediately exit on the second signal.
func ContextWithSignal(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	logger := klog.FromContext(ctx)

	signalChan := make(chan os.Signal, 2)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-signalChan
		logger.Info("Received shutdown signal, starting graceful shutdown...", "signal", sig)
		cancel()

		sig = <-signalChan
		logger.Info("Received second shutdown signal, forcing shutdown...", "signal", sig)
		klog.Flush()
		os.Exit(1)
	}()

	return ctx, cancel
}
