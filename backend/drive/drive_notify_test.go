package drive

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangeNotifyFailureThenResume(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		name := "first-page"
		if paginated {
			name = "later-page"
		}
		t.Run(name, func(t *testing.T) {
			failed := false
			var tokens []string
			f := newPermissionTestFs(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token := r.URL.Query().Get("pageToken")
				tokens = append(tokens, token)
				w.Header().Set("Content-Type", "application/json")
				if paginated && token == "previous" {
					_, _ = w.Write([]byte(`{"nextPageToken":"next"}`))
					return
				}
				if !failed {
					failed = true
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"code":503,"message":"poll failed"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"newStartPageToken":"resumed","changes":[{"fileId":"changed","file":{"name":"changed","mimeType":"text/plain"}}]}`))
			}))
			f.pacer.SetRetries(1)
			f.pacer.SetCalculator(pacer.NewDefault(pacer.MinSleep(0), pacer.MaxSleep(0)))
			f.dirCache = dircache.New("", "root", f)
			var notified []string
			notify := func(path string, kind fs.EntryType) {
				notified = append(notified, path)
				assert.Equal(t, fs.EntryObject, kind)
			}
			token, err := f.changeNotifyRunner(context.Background(), notify, "previous")
			require.Error(t, err)
			assert.Equal(t, "previous", token, "failed polls must keep the resume token")
			token, err = f.changeNotifyRunner(context.Background(), notify, token)
			require.NoError(t, err)
			assert.Equal(t, "resumed", token)
			assert.Equal(t, []string{"changed"}, notified)
			want := []string{"previous", "previous"}
			if paginated {
				want = []string{"previous", "next", "previous", "next"}
			}
			assert.Equal(t, want, tokens)
		})
	}
}

func TestChangeNotifyPermanentFailureRefreshesToken(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusGone} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			requests := make(chan string, 32)
			starts := 0
			f := newPermissionTestFs(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/startPageToken") {
					starts++
					requests <- "start"
					token := "previous"
					if starts > 1 {
						token = "fresh"
					}
					_, _ = fmt.Fprintf(w, `{"startPageToken":%q}`, token)
					return
				}
				token := r.URL.Query().Get("pageToken")
				requests <- token
				if token == "previous" {
					w.WriteHeader(code)
					_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"invalid page token"}}`, code)
					return
				}
				_, _ = w.Write([]byte(`{"newStartPageToken":"resumed","changes":[{"fileId":"changed","file":{"name":"changed","mimeType":"text/plain"}}]}`))
			}))
			f.pacer.SetCalculator(pacer.NewDefault(pacer.MinSleep(0), pacer.MaxSleep(0)))
			f.dirCache = dircache.New("", "root", f)
			notified := make(chan string, 32)
			intervals := make(chan time.Duration)
			defer close(intervals)
			f.ChangeNotify(context.Background(), func(path string, kind fs.EntryType) {
				assert.Equal(t, fs.EntryObject, kind)
				notified <- path
			}, intervals)
			intervals <- time.Millisecond
			for _, want := range []string{"start", "previous", "start", "fresh"} {
				select {
				case got := <-requests:
					require.Equal(t, want, got, "next poll must fetch a fresh start token after a permanent failure")
				case <-time.After(2 * time.Second):
					t.Fatal("timed out waiting for change notification request")
				}
			}
			select {
			case got := <-notified:
				assert.Equal(t, "changed", got)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for change notification after token refresh")
			}
		})
	}
}
