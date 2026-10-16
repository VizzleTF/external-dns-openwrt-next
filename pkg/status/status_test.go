package status

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/metrics"
)

const collection = "/apis/" + apiVersion + "/namespaces/dns/" + resource

// fakeAPI is the slice of a Kubernetes API server the client talks to.
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string]object
	tokens  []string
	calls   []string
	// refuse makes every write fail with this status.
	refuse int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))

	name, _ := strings.CutPrefix(r.URL.Path, collection+"/")
	f.calls = append(f.calls, r.Method+" "+name)

	if r.Method != http.MethodGet && f.refuse != 0 {
		w.WriteHeader(f.refuse)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"openwrtrecords is forbidden"}`))
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == collection:
		selector := r.URL.Query().Get("labelSelector")
		owner, ok := strings.CutPrefix(selector, managedByLabel+"="+managedBy+","+ownerLabel+"=")
		if !ok {
			f.t.Errorf("labelSelector = %q", selector)
		}
		items := slices.Collect(func(yield func(object) bool) {
			for _, obj := range f.objects {
				if obj.Metadata.Labels[ownerLabel] == owner && !yield(obj) {
					return
				}
			}
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})

	case r.Method == http.MethodPatch:
		if got := r.Header.Get("Content-Type"); got != "application/apply-patch+yaml" {
			f.t.Errorf("Content-Type = %q", got)
		}
		if q := r.URL.Query(); q.Get("fieldManager") != fieldManager || q.Get("force") != "true" {
			f.t.Errorf("query = %v", q)
		}
		var obj object
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &obj); err != nil {
			f.t.Errorf("apply body: %v", err)
		}
		if obj.APIVersion != apiVersion || obj.Kind != kind || obj.Metadata.Name != name ||
			obj.Metadata.Namespace != "dns" || obj.Metadata.Labels[managedByLabel] != managedBy || obj.Metadata.Labels[ownerLabel] == "" {
			f.t.Errorf("applied %s", body)
		}
		f.objects[name] = obj
		_, _ = w.Write(body)

	case r.Method == http.MethodDelete:
		if _, ok := f.objects[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.objects, name)

	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeAPI) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func newFake(t *testing.T) (*fakeAPI, *Syncer, string) {
	api := &fakeAPI{t: t, objects: map[string]object{}}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "one")

	return api, newSyncer(server, tokenFile, "homelab"), tokenFile
}

func newSyncer(server *httptest.Server, tokenFile, ownershipID string) *Syncer {
	syncer := NewSyncer(newClient(server.URL, "dns", ownershipID, tokenFile, server.Client()), slog.New(slog.DiscardHandler), metrics.NewRegistry())
	clock := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	syncer.now = func() time.Time { return clock }
	return syncer
}

func writeToken(t *testing.T, file, token string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func rec(name, state string, targets ...string) Record {
	return Record{DNSName: name, RecordType: "A", Targets: targets, State: state}
}

func TestSyncWritesOnlyWhatChanged(t *testing.T) {
	api, syncer, _ := newFake(t)
	ctx := context.Background()

	syncer.sync(ctx, []Record{rec("a.lan", Pending, "10.0.0.1"), rec("b.lan", Ready, "10.0.0.2")})
	if got, want := api.takeCalls(), []string{"GET " + collection, "PATCH a.a.lan", "PATCH a.b.lan"}; !slices.Equal(got, want) {
		t.Errorf("first sync: %v, want %v", got, want)
	}
	if obj, ok := api.objects["a.a.lan"]; !ok || obj.Status.State != Pending || obj.Status.LastTransitionTime != "2026-10-16T09:00:00Z" {
		t.Errorf("a.a.lan = %+v", obj)
	}

	syncer.now = func() time.Time { return time.Date(2026, 10, 16, 9, 5, 0, 0, time.UTC) }
	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1"), rec("b.lan", Ready, "10.0.0.2")})
	if got, want := api.takeCalls(), []string{"GET " + collection, "PATCH a.a.lan"}; !slices.Equal(got, want) {
		t.Errorf("second sync: %v, want %v", got, want)
	}
	if got := api.objects["a.a.lan"].Status.LastTransitionTime; got != "2026-10-16T09:05:00Z" {
		t.Errorf("lastTransitionTime = %s after a state change", got)
	}

	// A new target is no new state: the transition time stays.
	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.9")})
	if got, want := api.takeCalls(), []string{"GET " + collection, "PATCH a.a.lan", "DELETE a.b.lan"}; !slices.Equal(got, want) {
		t.Errorf("third sync: %v, want %v", got, want)
	}
	if obj := api.objects["a.a.lan"]; obj.Status.LastTransitionTime != "2026-10-16T09:05:00Z" || !slices.Equal(obj.Spec.Targets, []string{"10.0.0.9"}) {
		t.Errorf("a.a.lan = %+v", obj)
	}
}

func TestSyncPicksUpAfterARestart(t *testing.T) {
	api, syncer, _ := newFake(t)
	ctx := context.Background()
	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1"), rec("gone.lan", Ready, "10.0.0.2")})
	api.takeCalls()

	_, restarted, _ := newFake(t)
	restarted.client = syncer.client
	restarted.now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	restarted.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1")})

	// What the last process left that still holds is not rewritten, and what
	// no longer holds is deleted.
	if got, want := api.takeCalls(), []string{"GET " + collection, "DELETE a.gone.lan"}; !slices.Equal(got, want) {
		t.Errorf("after restart: %v, want %v", got, want)
	}
}

func TestSyncRestoresWhatWasDeletedByHand(t *testing.T) {
	api, syncer, _ := newFake(t)
	ctx := context.Background()
	records := []Record{rec("a.lan", Ready, "10.0.0.1")}

	syncer.sync(ctx, records)
	delete(api.objects, "a.a.lan")
	api.takeCalls()

	syncer.sync(ctx, records)
	if got, want := api.takeCalls(), []string{"GET " + collection, "PATCH a.a.lan"}; !slices.Equal(got, want) {
		t.Errorf("after a manual delete: %v, want %v", got, want)
	}
}

func TestSyncLeavesAnotherOwnersObjects(t *testing.T) {
	api, syncer, tokenFile := newFake(t)
	ctx := context.Background()
	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1")})

	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	other := newSyncer(server, tokenFile, "")
	other.sync(ctx, []Record{rec("b.lan", Ready, "10.0.0.2")})
	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1")})

	if _, ok := api.objects["a.b.lan"]; !ok {
		t.Error("an instance deleted the object of another")
	}
	if got := api.objects["a.b.lan"].Metadata.Labels[ownerLabel]; got != "none" {
		t.Errorf("owner label without ownership = %q, want none", got)
	}
}

func TestOwnerValue(t *testing.T) {
	if got := ownerValue(""); got != "none" {
		t.Errorf(`ownerValue("") = %q, want none`, got)
	}
	if got := ownerValue("homelab"); got != "homelab" {
		t.Errorf("ownerValue(homelab) = %q", got)
	}
	// A space is no label value: hashed.
	if got := ownerValue("home lab"); !labelValue.MatchString(got) || len(got) != 16 {
		t.Errorf("ownerValue(home lab) = %q, want a 16-character hash", got)
	}
}

func TestSyncSurvivesARefusingAPIServer(t *testing.T) {
	api, syncer, _ := newFake(t)
	api.refuse = http.StatusForbidden
	ctx := context.Background()

	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1")})
	if len(api.objects) != 0 {
		t.Fatalf("a refused write was stored")
	}
	err := syncer.client.apply(ctx, &object{})
	if err == nil || !strings.Contains(err.Error(), "openwrtrecords is forbidden") {
		t.Errorf("error = %v, want the API server's message", err)
	}

	// Retried with the next snapshot.
	api.refuse = 0
	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1")})
	if _, ok := api.objects["a.a.lan"]; !ok {
		t.Error("not written once the API server accepts it")
	}
}

func TestEveryRequestReadsTheTokenAgain(t *testing.T) {
	api, syncer, tokenFile := newFake(t)
	ctx := context.Background()

	syncer.sync(ctx, []Record{rec("a.lan", Ready, "10.0.0.1")})
	writeToken(t, tokenFile, "two")
	syncer.sync(ctx, []Record{rec("a.lan", Pending, "10.0.0.1")})

	if want := []string{"Bearer one", "Bearer one", "Bearer two", "Bearer two"}; !slices.Equal(api.tokens, want) {
		t.Errorf("tokens = %v, want %v", api.tokens, want)
	}
}

func TestRunWritesTheNewestSnapshot(t *testing.T) {
	api, syncer, _ := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncer.Run(ctx)
	}()

	syncer.Publish([]Record{rec("a.lan", Pending, "10.0.0.1")})
	syncer.Publish([]Record{rec("a.lan", Ready, "10.0.0.1")})

	deadline := time.Now().Add(5 * time.Second)
	for {
		api.mu.Lock()
		state := api.objects["a.a.lan"].Status.State
		api.mu.Unlock()
		if state == Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %q, want %s", state, Ready)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done
}

func TestObjectName(t *testing.T) {
	// A want ending in "-" stands for that prefix and a hash.
	for _, tc := range []struct{ recordType, name, want string }{
		{"A", "nas.lan", "a.nas.lan"},
		{"CNAME", "git.lan", "cname.git.lan"},
		{"A", "*.wild.lan", "a-"},
		{"A", "bad#.lan", "a-"},
		{"A", "-dash.lan", "a-"},
		{"A", "under_s.lan", "a-"},
		{"A", strings.Repeat("x", 251), "a." + strings.Repeat("x", 251)},
		{"A", strings.Repeat("x", 252), "a-"},
	} {
		record, want := Record{RecordType: tc.recordType, DNSName: tc.name}, tc.want
		got := objectName(record)
		if strings.HasSuffix(want, "-") {
			if !strings.HasPrefix(got, want) || len(got) != len(want)+16 {
				t.Errorf("objectName(%q) = %q, want a hash", record.DNSName, got)
			}
			continue
		}
		if got != want {
			t.Errorf("objectName(%q) = %q, want %q", record.DNSName, got, want)
		}
	}
}

func TestNewTrustsTheClusterCA(t *testing.T) {
	server := httptest.NewTLSServer(&fakeAPI{t: t, objects: map[string]object{}})
	t.Cleanup(server.Close)

	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	writeToken(t, filepath.Join(dir, "token"), "one")

	client, err := New(server.URL, filepath.Join(dir, "ca.crt"), filepath.Join(dir, "token"), "dns", "homelab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.list(context.Background()); err != nil {
		t.Errorf("list over TLS: %v", err)
	}

	if _, err := New(server.URL, filepath.Join(dir, "token"), filepath.Join(dir, "token"), "dns", "homelab"); err == nil {
		t.Error("a CA file holding no certificate was accepted")
	}
}
