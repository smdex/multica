package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestNativeSourceBorrowRetainsOwnerLockThroughEnrollmentShutdown(t *testing.T) {
	client, _ := newNSETestClient(t)
	hash, _ := nseProvision(t, "", client.baseURL)
	d := nseTestDaemon(t, client.baseURL)
	d.nseTrackRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.nativeSourceEnrollmentLoop(ctx); close(done) }()
	nseWaitFor(t, 6*time.Second, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.nativeSources[nseSourceID] != nil
	})
	if _, _, err := d.borrowNativeSource(ctx, nseSourceID, "wrong-marker"); err == nil {
		t.Fatal("wrong ownership marker allowed borrowing")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, _, err := d.borrowNativeSource(canceled, nseSourceID, hash); err == nil {
		t.Fatal("canceled caller borrowed the source")
	}
	domain, release, err := d.borrowNativeSource(ctx, nseSourceID, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cancel()
	// Observe shutdown's closing fence before checking both lifetime and
	// refusal. This is not a timing-dependent race against registry removal.
	nseWaitFor(t, time.Second, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.nativeSourcesClosing
	})
	select {
	case <-done:
		t.Fatal("enrollment shutdown released a borrowed domain")
	default:
	}
	if _, _, err := d.borrowNativeSource(context.Background(), nseSourceID, hash); err == nil {
		t.Fatal("shutdown permitted a new borrower")
	}
	if got, err := domain.ManifestHash(); err != nil || got != hash {
		t.Fatalf("borrowed domain closed before release: hash matches=%t err=%v", got == hash, err)
	}
	root, err := openNativeSourcesRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if competing, err := execenv.OpenNativeSource(root, domain.Identity()); !errors.Is(err, execenv.ErrNativeSourceBusy) {
		if competing != nil {
			competing.Close()
		}
		t.Fatalf("borrowed owner lock lost during shutdown: %v", err)
	}
	release()
	release() // The release callback is idempotent, not a second Done.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after final borrower released")
	}
	competing, err := execenv.OpenNativeSource(root, domain.Identity())
	if err != nil {
		t.Fatalf("owner lock not released after shutdown: %v", err)
	}
	competing.Close()
}
