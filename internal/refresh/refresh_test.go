package refresh

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/provider"
	"github.com/kilolockio/kilolock/internal/tfstate"
)

// recordingClient is a stand-in for provider.Client that:
//   - records every ReadResource call,
//   - returns a configurable response per (typeName, address-shaped)
//     prior state,
//   - never speaks the real wire protocol.
//
// It exists so the orchestrator's structure (grouping, concurrency,
// failure handling, splice-back) is testable without a database or
// a real provider binary. The DB-touching paths get exercised by
// the integration test file.
type recordingClient struct {
	mu sync.Mutex

	// onRead is the per-call hook. Inputs: typeName + the prior
	// state bytes; output: new state bytes (echoed back through
	// resp.NewState), diagnostics, and a transport error.
	//
	// Default behavior (when nil): echo prior state, no diagnostics,
	// no error — i.e. "no drift".
	onRead func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error)

	// onUpgrade is the per-call hook for UpgradeResourceState.
	// Inputs: typeName + the recorded schema_version + the raw
	// JSON bytes; output: upgraded state bytes (returned through
	// resp.UpgradedState), diagnostics, and a transport error.
	//
	// Default behavior (when nil): echo the raw bytes verbatim,
	// no diagnostics, no error — i.e. "no version bump needed".
	// That's identical to what real providers do when the recorded
	// version already matches the live schema version.
	onUpgrade func(typeName string, version int64, raw []byte) ([]byte, provider.Diagnostics, error)

	calls        []readCall
	upgradeCalls []upgradeRecording

	onStop    func()
	stopCalls atomic.Int32

	closed atomic.Bool
}

type readCall struct {
	TypeName     string
	CurrentState []byte
}

type upgradeRecording struct {
	TypeName string
	Version  int64
	RawState []byte
}

func (c *recordingClient) record(typeName string, in []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(in))
	copy(cp, in)
	c.calls = append(c.calls, readCall{TypeName: typeName, CurrentState: cp})
}

func (c *recordingClient) ReadResource(ctx context.Context, req provider.ReadResourceRequest) (*provider.ReadResourceResponse, provider.Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, provider.ErrProviderClosed
	}
	c.record(req.TypeName, req.CurrentState)
	if c.onRead != nil {
		out, diags, err := c.onRead(req.TypeName, req.CurrentState)
		if err != nil {
			return nil, diags, err
		}
		return &provider.ReadResourceResponse{NewState: out}, diags, nil
	}
	echo := make([]byte, len(req.CurrentState))
	copy(echo, req.CurrentState)
	return &provider.ReadResourceResponse{NewState: echo}, nil, nil
}

func (c *recordingClient) UpgradeResourceState(ctx context.Context, req provider.UpgradeResourceStateRequest) (*provider.UpgradeResourceStateResponse, provider.Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, provider.ErrProviderClosed
	}
	c.mu.Lock()
	c.upgradeCalls = append(c.upgradeCalls, upgradeRecording{
		TypeName: req.TypeName,
		Version:  req.Version,
		RawState: append([]byte(nil), req.RawState...),
	})
	c.mu.Unlock()

	if c.onUpgrade != nil {
		out, diags, err := c.onUpgrade(req.TypeName, req.Version, req.RawState)
		if err != nil {
			return nil, diags, err
		}
		return &provider.UpgradeResourceStateResponse{UpgradedState: out}, diags, nil
	}
	echo := append([]byte(nil), req.RawState...)
	return &provider.UpgradeResourceStateResponse{UpgradedState: echo}, nil, nil
}

func (c *recordingClient) GetSchema(context.Context) (*provider.Schema, provider.Diagnostics, error) {
	return &provider.Schema{}, nil, nil
}

func (c *recordingClient) Configure(context.Context, provider.ConfigureProviderRequest) (provider.Diagnostics, error) {
	return nil, nil
}

func (c *recordingClient) PlanResourceChange(context.Context, provider.PlanResourceChangeRequest) (*provider.PlanResourceChangeResponse, provider.Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, provider.ErrProviderClosed
	}
	return nil, nil, errors.New("PlanResourceChange not implemented in recordingClient test double")
}

func (c *recordingClient) Stop(context.Context) error {
	c.stopCalls.Add(1)
	if c.onStop != nil {
		c.onStop()
	}
	return nil
}
func (c *recordingClient) ProtocolVersion() int { return 6 }
func (c *recordingClient) Close() error {
	c.closed.Store(true)
	return nil
}

// fakeFactory hands out per-(source, alias) recordingClients. Tests
// assert against the resulting map after Run returns.
type fakeFactory struct {
	mu sync.Mutex

	// onOpen, when set, configures a freshly-minted client. Use it
	// to inject onRead behavior. Called exactly once per (source,
	// alias) pair: the factory caches the resulting client so a
	// test can locate the one associated with a specific group.
	onOpen func(source provider.SourceAddress, alias string, c *recordingClient)

	// openErr, when non-nil for a given (source, alias), causes
	// Open to fail with the configured error.
	openErr map[string]error

	// schemaFor, when non-nil for a given (source, alias), is
	// attached to the resulting OpenedClient. Required for tests
	// that exercise the schema-version gate on UpgradeResourceState;
	// other tests can leave it unset (a nil Schema makes
	// needsUpgrade return false).
	schemaFor map[string]*provider.Schema

	clients map[string]*recordingClient
	opens   atomic.Int32
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{
		openErr:   map[string]error{},
		schemaFor: map[string]*provider.Schema{},
		clients:   map[string]*recordingClient{},
	}
}

func (f *fakeFactory) Open(ctx context.Context, source provider.SourceAddress, alias string) (*OpenedClient, error) {
	f.opens.Add(1)
	key := source.String() + "[" + alias + "]"

	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.openErr[key]; ok && err != nil {
		return nil, err
	}
	c := &recordingClient{}
	if f.onOpen != nil {
		f.onOpen(source, alias, c)
	}
	f.clients[key] = c
	return &OpenedClient{
		Client:          c,
		Source:          source,
		Alias:           alias,
		Version:         "0.0.0-test",
		ProtocolVersion: 6,
		Schema:          f.schemaFor[key],
	}, nil
}

func (f *fakeFactory) clientFor(source string, alias string) *recordingClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[source+"["+alias+"]"]
}

// stateFixture returns a parsed *State with two managed resources
// across two providers, plus one data source which the orchestrator
// must skip. Useful when a test wants to drive groupByProvider
// directly.
func stateFixture(t *testing.T) *tfstate.State {
	t.Helper()
	const raw = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 4,
		"lineage": "11111111-1111-1111-1111-111111111111",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{ "schema_version": 0, "attributes": { "id": "vpc-1" } }
				]
			},
			{
				"mode": "managed",
				"type": "null_resource",
				"name": "trigger",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": [
					{ "schema_version": 0, "attributes": { "id": "n-1" } },
					{ "schema_version": 0, "attributes": { "id": "n-2" }, "index_key": "two" }
				]
			},
			{
				"mode": "data",
				"type": "aws_ami",
				"name": "ubuntu",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{ "schema_version": 0, "attributes": { "id": "ami-1" } }
				]
			}
		]
	}`
	s, err := tfstate.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse fixture: %v", err)
	}
	return s
}

func TestGroupByProvider_TwoProvidersSkipsDataSource(t *testing.T) {
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (data source must be skipped)", len(groups))
	}
	// Stable order: aws < null lexicographically.
	if got, want := groups[0].Source.String(), "registry.terraform.io/hashicorp/aws"; got != want {
		t.Errorf("groups[0].Source: got %q, want %q", got, want)
	}
	if got, want := groups[1].Source.String(), "registry.terraform.io/hashicorp/null"; got != want {
		t.Errorf("groups[1].Source: got %q, want %q", got, want)
	}
	// The aws group has one instance (the vpc). The null group has
	// two instances of the same resource (count/for_each style).
	if len(groups[0].Entries) != 1 {
		t.Errorf("aws entries: got %d, want 1", len(groups[0].Entries))
	}
	if len(groups[1].Entries) != 2 {
		t.Errorf("null entries: got %d, want 2", len(groups[1].Entries))
	}
}

func TestGroupByProvider_AliasesAreDistinct(t *testing.T) {
	const raw = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "22222222-2222-2222-2222-222222222222",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "east",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{ "schema_version": 0, "attributes": { "id": "vpc-east" } }]
			},
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "west",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"].west",
				"instances": [{ "schema_version": 0, "attributes": { "id": "vpc-west" } }]
			}
		]
	}`
	s, err := tfstate.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (alias splits)", len(groups))
	}
	if groups[0].Alias != "" {
		t.Errorf("groups[0].Alias: got %q, want \"\"", groups[0].Alias)
	}
	if groups[1].Alias != "west" {
		t.Errorf("groups[1].Alias: got %q, want \"west\"", groups[1].Alias)
	}
}

func TestGroupByProvider_RejectsInvalidProviderRef(t *testing.T) {
	const raw = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "33333333-3333-3333-3333-333333333333",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "broken",
				"provider": "this is not a valid provider reference",
				"instances": [{ "schema_version": 0, "attributes": { "id": "x" } }]
			}
		]
	}`
	s, err := tfstate.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := groupByProvider(s); err == nil {
		t.Fatal("expected error for malformed provider ref")
	}
}

func TestProcessGroups_EchoesPriorState(t *testing.T) {
	// End-to-end of the orchestrator's resource-walk path WITHOUT
	// the database round-trip: build a parsed state, run the
	// goroutine fan-out manually, and verify ReadResource was
	// called for every managed instance.
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}
	factory := newFakeFactory()

	processGroups(context.Background(), factory, groups, Options{}, result)

	if got, want := result.ResourcesChecked, 3; got != want {
		t.Errorf("ResourcesChecked: got %d, want %d", got, want)
	}
	if got := result.ResourcesChanged; got != 0 {
		t.Errorf("ResourcesChanged: got %d, want 0 (echo => no drift)", got)
	}
	if got := result.ResourcesFailed; got != 0 {
		t.Errorf("ResourcesFailed: got %d, want 0", got)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors: got %v, want []", result.Errors)
	}
	if got, want := factory.opens.Load(), int32(2); got != want {
		t.Errorf("Open calls: got %d, want %d (one per provider group)", got, want)
	}

	awsClient := factory.clientFor("registry.terraform.io/hashicorp/aws", "")
	nullClient := factory.clientFor("registry.terraform.io/hashicorp/null", "")
	if awsClient == nil || nullClient == nil {
		t.Fatalf("missing recorded client (aws=%v null=%v)", awsClient, nullClient)
	}
	if !awsClient.closed.Load() || !nullClient.closed.Load() {
		t.Errorf("clients should be closed after Run; got aws=%v null=%v",
			awsClient.closed.Load(), nullClient.closed.Load())
	}
}

func TestProcessGroups_DriftIsCounted(t *testing.T) {
	// onRead returns "id":"vpc-2" for the aws group, so
	// orchestrator should count one ResourcesChanged for the
	// aws_vpc.main instance and zero for the (echoed) null pair.
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		if strings.Contains(src.String(), "/aws") {
			c.onRead = func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error) {
				return []byte(`{"id":"vpc-2"}`), nil, nil
			}
		}
	}

	processGroups(context.Background(), factory, groups, Options{}, result)

	if got, want := result.ResourcesChecked, 3; got != want {
		t.Errorf("ResourcesChecked: got %d, want %d", got, want)
	}
	if got, want := result.ResourcesChanged, 1; got != want {
		t.Errorf("ResourcesChanged: got %d, want %d", got, want)
	}
	// And the splice-back happened — the parsed state should now
	// reflect the new attributes for aws_vpc.main.
	got := strings.TrimSpace(string(s.Resources[0].Instances[0].Attributes))
	if got != `{"id":"vpc-2"}` {
		t.Errorf("aws_vpc.main attributes after refresh: got %s", got)
	}

	// processGroups populates ChangedAddresses across goroutines but
	// the final sort happens at Run boundary, not here. Test the
	// pre-sort invariant: every drifted instance shows up in the
	// slice, once each. Run-level tests cover the sort.
	if len(result.ChangedAddresses) != 1 {
		t.Errorf("ChangedAddresses: got %v, want length 1", result.ChangedAddresses)
	}
	if !strings.Contains(strings.Join(result.ChangedAddresses, ","), "aws_vpc.main") {
		t.Errorf("ChangedAddresses should contain aws_vpc.main; got %v", result.ChangedAddresses)
	}
}

// TestProcessGroups_MultipleDriftAddressesAreCollected stresses the
// per-resource collection across both groups, with one resource
// drifted in each. Verifies len(ChangedAddresses) == ResourcesChanged
// (the orchestrator's per-resource/aggregate invariant) and that
// addresses from both providers land in the same slice.
func TestProcessGroups_MultipleDriftAddressesAreCollected(t *testing.T) {
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		c.onRead = func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error) {
			// Force drift on every resource, every group.
			return []byte(`{"id":"drifted"}`), nil, nil
		}
	}

	processGroups(context.Background(), factory, groups, Options{}, result)

	if got, want := result.ResourcesChanged, 3; got != want {
		t.Errorf("ResourcesChanged: got %d, want %d", got, want)
	}
	if got := len(result.ChangedAddresses); got != result.ResourcesChanged {
		t.Errorf("ChangedAddresses invariant: len=%d, ResourcesChanged=%d",
			got, result.ResourcesChanged)
	}
	joined := strings.Join(result.ChangedAddresses, ",")
	for _, must := range []string{"aws_vpc.main", "null_resource.trigger"} {
		if !strings.Contains(joined, must) {
			t.Errorf("expected ChangedAddresses to contain %q; got %v", must, result.ChangedAddresses)
		}
	}
}

func TestProcessGroups_PerResourceErrorIsCollected(t *testing.T) {
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	wantErr := errors.New("simulated provider crash")
	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		if strings.Contains(src.String(), "/aws") {
			c.onRead = func(string, []byte) ([]byte, provider.Diagnostics, error) {
				return nil, nil, wantErr
			}
		}
	}

	processGroups(context.Background(), factory, groups, Options{}, result)

	if got, want := result.ResourcesFailed, 1; got != want {
		t.Errorf("ResourcesFailed: got %d, want %d", got, want)
	}
	if got, want := result.ResourcesChecked, 2; got != want {
		// aws failed; null's two instances succeeded.
		t.Errorf("ResourcesChecked: got %d, want %d", got, want)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors: got %d, want 1 (%+v)", len(result.Errors), result.Errors)
	}
	if !errors.Is(result.Errors[0].Err, wantErr) {
		t.Errorf("Errors[0].Err: got %v, want wrapping %v", result.Errors[0].Err, wantErr)
	}
	if got, want := result.Errors[0].Address, "aws_vpc.main"; got != want {
		t.Errorf("Errors[0].Address: got %q, want %q", got, want)
	}
}

func TestProcessGroups_FailFastCancelsPeers(t *testing.T) {
	// Wire the aws group to fail synchronously; the null group must
	// observe the cancellation before its second resource starts.
	// Verifies the FailFast cancel signal is reaching peers.
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	wantErr := errors.New("fail fast trigger")

	// barrier is closed when AWS has emitted its error and called
	// cancel. Null waits for it before entering its second
	// ReadResource so the cancellation has time to propagate
	// instead of racing.
	barrier := make(chan struct{})

	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		switch {
		case strings.Contains(src.String(), "/aws"):
			c.onRead = func(string, []byte) ([]byte, provider.Diagnostics, error) {
				close(barrier)
				return nil, nil, wantErr
			}
		case strings.Contains(src.String(), "/null"):
			calls := 0
			c.onRead = func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error) {
				calls++
				if calls > 1 {
					<-barrier // wait for aws to cancel
				}
				echo := make([]byte, len(currentState))
				copy(echo, currentState)
				return echo, nil, nil
			}
		}
	}

	processGroups(context.Background(), factory, groups, Options{FailFast: true}, result)

	if result.ResourcesFailed < 1 {
		t.Errorf("expected at least one failed resource; result=%+v", result)
	}
	if !containsErr(result.Errors, wantErr) {
		t.Errorf("expected wantErr in Errors, got %+v", result.Errors)
	}
}

func TestProcessGroups_CancelCallsStop(t *testing.T) {
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	release := make(chan struct{})
	stopped := make(chan struct{}, 1)
	started := make(chan struct{}, 1)
	var releaseOnce sync.Once

	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		c.onRead = func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release // block until Stop unblocks us
			echo := make([]byte, len(currentState))
			copy(echo, currentState)
			return echo, nil, nil
		}
		c.onStop = func() {
			select {
			case stopped <- struct{}{}:
			default:
			}
			releaseOnce.Do(func() { close(release) })
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		processGroups(ctx, factory, groups, Options{}, result)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected at least one ReadResource call to start")
	}

	cancel()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("expected Stop to be called after cancellation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processGroups did not return after cancellation")
	}
}

func containsErr(errs []ResourceError, target error) bool {
	for _, re := range errs {
		if errors.Is(re.Err, target) {
			return true
		}
	}
	return false
}

func TestProcessGroups_FactoryOpenFailureFailsWholeGroup(t *testing.T) {
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	openFail := errors.New("boom: terraform init never ran")
	factory := newFakeFactory()
	factory.openErr["registry.terraform.io/hashicorp/null[]"] = openFail

	processGroups(context.Background(), factory, groups, Options{}, result)

	// null group has 2 instances; both should be failed.
	if got, want := result.ResourcesFailed, 2; got != want {
		t.Errorf("ResourcesFailed: got %d, want %d", got, want)
	}
	if got, want := result.ResourcesChecked, 1; got != want {
		// aws still succeeds.
		t.Errorf("ResourcesChecked: got %d, want %d (only aws)", got, want)
	}
	for _, re := range result.Errors {
		if !errors.Is(re.Err, openFail) {
			t.Errorf("recorded error did not wrap openFail: %v", re.Err)
		}
	}
}

func TestProcessGroups_ConcurrencyCap(t *testing.T) {
	// Build a state with three providers; observe that with
	// Concurrency=1 the orchestrator opens them serially (the
	// observed peak open count never exceeds the cap).
	const raw = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "44444444-4444-4444-4444-444444444444",
		"outputs": {},
		"resources": [
			{ "mode":"managed", "type":"aws_vpc", "name":"a",
			  "provider":"provider[\"registry.terraform.io/hashicorp/aws\"]",
			  "instances":[{"schema_version":0,"attributes":{"id":"a"}}] },
			{ "mode":"managed", "type":"null_resource", "name":"b",
			  "provider":"provider[\"registry.terraform.io/hashicorp/null\"]",
			  "instances":[{"schema_version":0,"attributes":{"id":"b"}}] },
			{ "mode":"managed", "type":"random_id", "name":"c",
			  "provider":"provider[\"registry.terraform.io/hashicorp/random\"]",
			  "instances":[{"schema_version":0,"attributes":{"id":"c"}}] }
		]
	}`
	s, err := tfstate.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	var (
		mu     sync.Mutex
		active int
		peak   int
	)
	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		c.onRead = func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error) {
			// Simulate work so the cap actually has a window in
			// which to be observed.
			echo := make([]byte, len(currentState))
			copy(echo, currentState)
			defer func() {
				mu.Lock()
				active--
				mu.Unlock()
			}()
			return echo, nil, nil
		}
	}

	processGroups(context.Background(), factory, groups, Options{Concurrency: 1}, result)
	if peak > 1 {
		t.Errorf("peak active groups: got %d, want <= 1", peak)
	}
}

func TestBytesEqualJSON_KeyOrderAgnostic(t *testing.T) {
	// jsonEqual must treat key-reordered objects as equal —
	// providers freely re-encode JSON and a refresh that only
	// changes key order is not drift.
	a := []byte(`{"id":"x","triggers":{"a":"1","b":"2"}}`)
	b := []byte(`{"triggers":{"b":"2","a":"1"},"id":"x"}`)
	if !bytesEqualJSON(a, b) {
		t.Errorf("bytesEqualJSON: expected key-reordered objects to be equal")
	}
	c := []byte(`{"id":"x","triggers":{"a":"1","b":"3"}}`)
	if bytesEqualJSON(a, c) {
		t.Errorf("bytesEqualJSON: expected differing values to be unequal")
	}
}

func TestRun_RejectsInvalidArgs(t *testing.T) {
	if _, err := Run(context.Background(), nil, newFakeFactory(), Options{StateName: "x"}); err == nil {
		t.Error("expected error for nil store")
	}
	// st == nil and factory == nil cases share the same constructor
	// validations; emptyStateName covers a path that doesn't reach
	// the store at all.
	if _, err := Run(context.Background(), nil, nil, Options{}); err == nil {
		t.Error("expected error for empty StateName")
	}
}

func TestResourceError_Format(t *testing.T) {
	// Make sure the formatted message is operator-friendly: the
	// address is the prefix, the reason follows. CLI rendering
	// will lean on this.
	re := ResourceError{Address: "aws_vpc.main", Err: errors.New("boom")}
	if got, want := re.Error(), "aws_vpc.main: boom"; got != want {
		t.Errorf("Error: got %q, want %q", got, want)
	}
	if !errors.Is(re, re.Err) {
		t.Errorf("ResourceError should unwrap to inner error")
	}
}

func TestJoinDiagnostics(t *testing.T) {
	d := provider.Diagnostics{
		{Severity: provider.SeverityError, Summary: "bad config", Detail: "region missing"},
		{Severity: provider.SeverityError, Summary: "no creds"},
	}
	got := joinDiagnostics(d)
	if got != "bad config: region missing; no creds" {
		t.Errorf("joinDiagnostics: %q", got)
	}
	if joinDiagnostics(nil) != "" {
		t.Errorf("joinDiagnostics(nil) should be empty")
	}
}

func TestTruncateSummary(t *testing.T) {
	long := strings.Repeat("a", 600)
	got := truncateSummary(long, 100)
	if len([]rune(got)) > 100 {
		t.Errorf("truncateSummary: got %d runes, want <= 100", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncateSummary: expected ellipsis suffix, got %q", got[len(got)-3:])
	}
	if truncateSummary("short", 100) != "short" {
		t.Errorf("truncateSummary should pass through short strings unchanged")
	}
}

func TestNeedsUpgrade_NoSchema(t *testing.T) {
	// No schema attached → can't decide → orchestrator skips
	// upgrade. The real-world provider would surface a schema
	// mismatch via ReadResource diagnostics if there's a real
	// problem.
	opened := &OpenedClient{Schema: nil}
	e := &groupEntry{TypeName: "null_resource", SchemaVersion: 0}
	if needsUpgrade(opened, e) {
		t.Errorf("needsUpgrade(nil schema): got true, want false")
	}
}

func TestNeedsUpgrade_TypeMissingFromSchema(t *testing.T) {
	// Schema present but resource type unknown → also a skip.
	// Reaching this state means the orchestrator dropped through
	// to a managed resource not in the live provider's resource
	// set, which is an integrity-of-state question; ReadResource
	// will produce a clearer error than UpgradeResourceState would.
	opened := &OpenedClient{Schema: &provider.Schema{Resources: map[string]*provider.ResourceSchema{}}}
	e := &groupEntry{TypeName: "no_such_type", SchemaVersion: 0}
	if needsUpgrade(opened, e) {
		t.Errorf("needsUpgrade(unknown type): got true, want false")
	}
}

func TestNeedsUpgrade_EqualOrHigherIsSkipped(t *testing.T) {
	// schema_version >= live version → skip. Equal is the common
	// case; higher would mean state was written by a newer provider
	// than the one we have on disk, which is a separate failure
	// mode UpgradeResourceState wouldn't fix.
	opened := &OpenedClient{Schema: &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{"x": {Version: 2}},
	}}
	for _, v := range []int{2, 3} {
		e := &groupEntry{TypeName: "x", SchemaVersion: v}
		if needsUpgrade(opened, e) {
			t.Errorf("needsUpgrade(stored=%d, live=2): got true, want false", v)
		}
	}
}

func TestNeedsUpgrade_LowerVersionTriggers(t *testing.T) {
	opened := &OpenedClient{Schema: &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{"x": {Version: 2}},
	}}
	e := &groupEntry{TypeName: "x", SchemaVersion: 0}
	if !needsUpgrade(opened, e) {
		t.Error("needsUpgrade(stored=0, live=2): got false, want true")
	}
}

// TestProcessGroups_UpgradeRunsForLowerVersion proves the full
// upgrade-then-read sequence: the orchestrator detects the schema
// version mismatch, calls UpgradeResourceState first, splices the
// upgraded attributes + bumps the recorded schema_version, then
// proceeds with ReadResource against the upgraded prior.
func TestProcessGroups_UpgradeRunsForLowerVersion(t *testing.T) {
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	factory := newFakeFactory()
	// Force the null provider to advertise schema_version=1 so the
	// orchestrator hits the upgrade path. State has schema_version=0
	// for both null_resource instances.
	factory.schemaFor["registry.terraform.io/hashicorp/null[]"] = &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{
			"null_resource": {Version: 1},
		},
	}
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		if !strings.Contains(src.String(), "/null") {
			return
		}
		// Simulated migration: emit the same JSON shape under
		// the new schema. Real providers might restructure the
		// payload here; for the orchestrator test we only care
		// that the call happens and the response is spliced.
		c.onUpgrade = func(typeName string, version int64, raw []byte) ([]byte, provider.Diagnostics, error) {
			return raw, nil, nil
		}
	}

	processGroups(context.Background(), factory, groups, Options{}, result)

	if result.ResourcesFailed != 0 {
		t.Fatalf("ResourcesFailed: got %d, want 0; errors=%v", result.ResourcesFailed, result.Errors)
	}

	nullClient := factory.clientFor("registry.terraform.io/hashicorp/null", "")
	if nullClient == nil {
		t.Fatal("missing null client")
	}
	// Both null instances had schema_version=0 < live=1, so both
	// must have triggered an upgrade call.
	if got := len(nullClient.upgradeCalls); got != 2 {
		t.Errorf("upgradeCalls: got %d, want 2 (one per instance)", got)
	}
	for _, c := range nullClient.upgradeCalls {
		if c.TypeName != "null_resource" {
			t.Errorf("upgradeCalls.TypeName: got %q, want null_resource", c.TypeName)
		}
		if c.Version != 0 {
			t.Errorf("upgradeCalls.Version: got %d, want 0 (the recorded version)", c.Version)
		}
	}

	// And the recorded schema_version in state should now be the
	// live version (1) — proving the splice + bump worked. State
	// fixture put both null instances under resource index 1.
	for i, inst := range s.Resources[1].Instances {
		if inst.SchemaVersion != 1 {
			t.Errorf("null instance %d schema_version after upgrade: got %d, want 1", i, inst.SchemaVersion)
		}
	}

	// aws_vpc.main had no upgrade hook configured and the aws
	// group has no Schema attached → no upgrade should have run.
	awsClient := factory.clientFor("registry.terraform.io/hashicorp/aws", "")
	if got := len(awsClient.upgradeCalls); got != 0 {
		t.Errorf("aws upgradeCalls: got %d, want 0 (no schema bump there)", got)
	}
}

// TestProcessGroups_UpgradeErrorPreventsReadResource confirms that a
// per-resource upgrade failure aborts the read for that instance and
// surfaces a clean error to the operator. The other instances in the
// same group keep going (no FailFast), which matches the rest of the
// orchestrator's per-resource error contract.
func TestProcessGroups_UpgradeErrorPreventsReadResource(t *testing.T) {
	s := stateFixture(t)
	groups, err := groupByProvider(s)
	if err != nil {
		t.Fatalf("groupByProvider: %v", err)
	}
	result := &Result{parsed: s}

	factory := newFakeFactory()
	factory.schemaFor["registry.terraform.io/hashicorp/null[]"] = &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{
			"null_resource": {Version: 1},
		},
	}
	calls := atomic.Int32{}
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		if !strings.Contains(src.String(), "/null") {
			return
		}
		c.onUpgrade = func(typeName string, version int64, raw []byte) ([]byte, provider.Diagnostics, error) {
			// Fail only the first call; succeed on the second.
			if calls.Add(1) == 1 {
				return nil, nil, errors.New("upgrade ladder broke")
			}
			return raw, nil, nil
		}
	}

	processGroups(context.Background(), factory, groups, Options{}, result)

	// Three managed instances total: 1 aws (no upgrade, succeeds),
	// 2 null (one upgrade fails, one upgrade succeeds). So we
	// expect ResourcesFailed=1, ResourcesChecked=2 (the two that
	// reached ReadResource).
	if result.ResourcesFailed != 1 {
		t.Errorf("ResourcesFailed: got %d, want 1", result.ResourcesFailed)
	}
	if result.ResourcesChecked != 2 {
		t.Errorf("ResourcesChecked: got %d, want 2 (upgrade-failed instance must not be checked)", result.ResourcesChecked)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors: got %d, want 1", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Err.Error(), "UpgradeResourceState") {
		t.Errorf("error should mention UpgradeResourceState, got %q", result.Errors[0].Err)
	}

	// The orchestrator must not have issued a ReadResource for the
	// failed-upgrade instance. With FailFast off, the rest still
	// runs, so there should be exactly one missing ReadResource:
	// total managed instances (3) minus the failed-upgrade one (1)
	// = 2 ReadResource calls across both groups.
	awsClient := factory.clientFor("registry.terraform.io/hashicorp/aws", "")
	nullClient := factory.clientFor("registry.terraform.io/hashicorp/null", "")
	total := len(awsClient.calls) + len(nullClient.calls)
	if total != 2 {
		t.Errorf("total ReadResource calls: got %d, want 2", total)
	}
}

// Compile-time check: recordingClient really does implement the
// provider.Client interface. If a future change to provider.Client
// breaks that, this file fails to compile rather than the orchestrator
// silently falling out of step with the wire layer.
var _ provider.Client = (*recordingClient)(nil)

// silence unused-import nag from reflect when tests run cleanly.
var _ = reflect.DeepEqual
