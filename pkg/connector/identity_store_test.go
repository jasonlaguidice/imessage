package connector

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// TestSessionStateConcurrentAccess saves and loads session.json from many
// goroutines at once, as concurrent logins do. Every load must see one
// complete state, never a truncated or interleaved file.
func TestSessionStateConcurrentAccess(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	log := zerolog.Nop()
	payload := strings.Repeat("x", 256*1024)
	saveSessionState(log, PersistedSessionState{IDSIdentity: "seed", APSState: payload})

	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				saveSessionState(log, PersistedSessionState{IDSIdentity: fmt.Sprintf("w%d-%d", i, j), APSState: payload})
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if st := loadSessionState(log); st.IDSIdentity == "" || st.APSState != payload {
					errs <- fmt.Errorf("load %d saw an incomplete state (identity=%q, aps len=%d)", j, st.IDSIdentity, len(st.APSState))
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
