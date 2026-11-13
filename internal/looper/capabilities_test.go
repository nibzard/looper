package looper

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// capabilitiesSchema reads the published contract schema so the documents
// the CLI really produces are checked against it, not just the examples.
func capabilitiesSchema(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(legacyContractDocs, "schemas", "v2", "capabilities.schema.json"))
	if err != nil {
		t.Skipf("capabilities schema is absent: %v", err)
	}
	return b
}

// writeControllerMetadata publishes a controller record for tests. Passing
// nil capabilities writes the pre-contract shape an older controller emits.
func writeControllerMetadata(t *testing.T, dir, session string, caps *ControllerCapabilities) *ControllerInfo {
	t.Helper()
	info := controllerMetadata(dir, session, caps)
	if err := writeJSON(controllerPath(dir), info); err != nil {
		t.Fatal(err)
	}
	return info
}

// writeControllerMetadataFile is writeControllerMetadata for goroutines that
// must not call t.Fatal after their test returned; the caller reports the
// error from the test goroutine.
func writeControllerMetadataFile(dir, session string, caps *ControllerCapabilities) error {
	return writeJSON(controllerPath(dir), controllerMetadata(dir, session, caps))
}

func controllerMetadata(dir, session string, caps *ControllerCapabilities) *ControllerInfo {
	info := &ControllerInfo{
		Version: ControllerVersion, Session: session, PID: os.Getpid(),
		Root: dir, Mode: ControllerModeForeground, Started: "2026-11-05T09:00:00Z",
		Build: &BuildInfo{Release: "0.10.0", Revision: "deadbeef", Modified: false},
	}
	if caps != nil {
		info.Capabilities = caps
	}
	return info
}

func TestPublishControllerAdvertisesCapabilities(t *testing.T) {
	dir := t.TempDir()
	info, err := publishController(dir, ControllerModeDetached, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if info.Capabilities == nil {
		t.Fatal("published identity carries no capability record")
	}
	want := controllerRequestTypes()
	if strings.Join(info.Capabilities.RequestTypes, ",") != strings.Join(want, ",") {
		t.Fatalf("request types %v, want %v", info.Capabilities.RequestTypes, want)
	}
	if joinInts(info.Capabilities.ContractVersions) != "1, 2" {
		t.Fatalf("contract versions %v", info.Capabilities.ContractVersions)
	}
	if info.Capabilities.Limits["pending_requests"] != maxPendingRequests {
		t.Fatalf("pending limit %v", info.Capabilities.Limits)
	}
	// The record must survive the read path other commands use.
	read, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	if read.Capabilities == nil || len(read.Capabilities.RequestTypes) == 0 {
		t.Fatal("readback lost the capability record")
	}
}

func TestControllerMetadataWithoutCapabilitiesStaysReadable(t *testing.T) {
	dir := t.TempDir()
	writeControllerMetadata(t, dir, "run-old", nil)
	info, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Capabilities != nil {
		t.Fatal("a pre-contract record must keep the capability field absent, not synthesized")
	}
	types, absent := advertisedRequestTypes(info)
	if !absent {
		t.Fatal("absent metadata must be reported as absent")
	}
	if strings.Join(types, ",") != strings.Join(legacyBaseRequestTypes, ",") {
		t.Fatalf("derived types %v", types)
	}
}

func TestValidateControllerRequest(t *testing.T) {
	full := newControllerCapabilities()
	noStop := &ControllerCapabilities{ContractVersions: []int{1}, RequestTypes: []string{
		PublicRequestAddTask, PublicRequestAddPlan, PublicRequestSettings, PublicRequestRequeue,
	}}
	cases := []struct {
		name       string
		info       *ControllerInfo
		request    string
		wantErr    string
		wantAbsent bool
	}{
		{"no metadata decides nothing", nil, PublicRequestAddTask, "", false},
		{"advertised type passes", &ControllerInfo{Session: "run-a", Capabilities: full}, PublicRequestAddTask, "", false},
		{"unadvertised type refused", &ControllerInfo{Session: "run-a", Capabilities: noStop}, PublicRequestStop, "does not accept", false},
		{"absent metadata derives the base set", &ControllerInfo{Session: "run-old"}, PublicRequestRequeue, "", false},
		{"future type refused on absent metadata", &ControllerInfo{Session: "run-old"}, "cancel-task", "predates capability advertisement", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateControllerRequest(tc.info, tc.request)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "No request was queued") &&
				!strings.Contains(err.Error(), "not submitted") {
				t.Fatalf("error %q does not state that nothing was queued", err)
			}
			unsupported, ok := err.(*unsupportedRequestError)
			if !ok || unsupported.absentMetadata != tc.wantAbsent {
				t.Fatalf("error is not the specific incompatibility result: %v", err)
			}
			if unsupported.session != tc.info.Session {
				t.Fatalf("error names session %q, want %q", unsupported.session, tc.info.Session)
			}
		})
	}
}

// A newer client must not leave a dead letter at an older controller: when
// the live controller does not accept the request type, publication is
// refused and no envelope reaches the request directory.
func TestSubmitRefusesUnsupportedRequestWithoutEnvelope(t *testing.T) {
	dir := t.TempDir()
	older := &ControllerCapabilities{ContractVersions: []int{1}, RequestTypes: []string{
		PublicRequestAddPlan, PublicRequestSettings, PublicRequestRequeue, PublicRequestStop,
	}}
	info := writeControllerMetadata(t, dir, "run-old", older)
	_, err := submitSettingsRequest(dir, SettingsRequest{
		Version: SettingsRequestVersion, ID: "add-1", Type: RequestTypeAddTask,
		Session: info.Session, Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}},
	}, 300*time.Millisecond)
	if err == nil {
		t.Fatal("submitting an unsupported request type must fail")
	}
	var unsupported *unsupportedRequestError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error is not the specific incompatibility result: %v", err)
	}
	if !strings.Contains(err.Error(), PublicRequestAddTask) || !strings.Contains(err.Error(), info.Session) {
		t.Fatalf("error %q names neither the request type nor the session", err)
	}
	entries, readErr := os.ReadDir(requestDir(dir))
	if readErr == nil && len(entries) > 0 {
		t.Fatalf("no envelope may be queued, found %d", len(entries))
	}
	if _, ok := readRequestAck(dir, "add-1"); ok {
		t.Fatal("no acknowledgement may exist for a refused request")
	}
}

// An envelope type this client does not know cannot be validated at all, so
// it is refused the same way instead of being published speculatively.
func TestSubmitRefusesUnknownEnvelopeType(t *testing.T) {
	dir := t.TempDir()
	info := writeControllerMetadata(t, dir, "run-now", newControllerCapabilities())
	_, err := submitSettingsRequest(dir, SettingsRequest{
		Version: SettingsRequestVersion, ID: "cancel-1", Type: "cancel-task", Session: info.Session,
	}, 300*time.Millisecond)
	if err == nil {
		t.Fatal("an unknown envelope type must be refused")
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), "cancel-1.json")); statErr == nil {
		t.Fatal("no envelope may be queued")
	}
}

// Plan imports survive controller restarts, so a session change must not
// refuse them; an advertised set without the type still must.
func TestSubmitPlanImportCapabilityRules(t *testing.T) {
	dir := t.TempDir()
	writeControllerMetadata(t, dir, "run-a", newControllerCapabilities())
	plan := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A request naming a session that no longer owns the repository still
	// publishes: the next controller replays the import.
	if _, err := submitPlanImport(dir, "run-gone", "", []string{plan}); err != nil {
		t.Fatalf("plan import refused on session change: %v", err)
	}
	os.Remove(filepath.Join(requestDir(dir), mustFirstEntry(t, requestDir(dir))))
	withoutImport := &ControllerCapabilities{ContractVersions: []int{1}, RequestTypes: []string{
		PublicRequestAddTask, PublicRequestSettings, PublicRequestRequeue, PublicRequestStop,
	}}
	writeControllerMetadata(t, dir, "run-old", withoutImport)
	if _, err := submitPlanImport(dir, "run-old", "", []string{plan}); err == nil {
		t.Fatal("plan import must be refused when the advertised set excludes it")
	} else if _, statErr := os.Stat(filepath.Join(requestDir(dir))); statErr == nil {
		if entries, _ := os.ReadDir(requestDir(dir)); len(entries) > 0 {
			t.Fatal("no envelope may be queued")
		}
	}
}

// The stale-session race at publication: the controller changed between the
// caller reading identity and the envelope being written. The request is
// refused before publication with an actionable outcome.
func TestSubmitRefusesStaleSessionBeforePublication(t *testing.T) {
	dir := t.TempDir()
	writeControllerMetadata(t, dir, "run-new", newControllerCapabilities())
	_, err := submitSettingsRequest(dir, SettingsRequest{
		Version: SettingsRequestVersion, ID: "stale-1", Type: RequestTypeSettings, Session: "run-old",
	}, 300*time.Millisecond)
	var stale *staleSessionError
	if !errors.As(err, &stale) {
		t.Fatalf("error is not the stale-session outcome: %v", err)
	}
	if !strings.Contains(err.Error(), "run-old") || !strings.Contains(err.Error(), "run-new") {
		t.Fatalf("error %q names neither session", err)
	}
	if !strings.Contains(err.Error(), "Resubmit") {
		t.Fatalf("error %q gives no next step", err)
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), "stale-1.json")); statErr == nil {
		t.Fatal("no envelope may be queued")
	}
}

// The stale-session race during the wait: the owning controller exits and a
// replacement starts while the submitter waits. The wait reports the race
// instead of sleeping to its timeout.
func TestSubmitReportsSessionChangeDuringWait(t *testing.T) {
	dir := t.TempDir()
	writeControllerMetadata(t, dir, "run-a", newControllerCapabilities())
	started := time.Now()
	errCh := make(chan error, 1)
	go func() {
		_, err := submitSettingsRequest(dir, SettingsRequest{
			Version: SettingsRequestVersion, ID: "stale-2", Type: RequestTypeSettings, Session: "run-a",
		}, 10*time.Second)
		errCh <- err
	}()
	// The controller exits and a replacement publishes before the timeout.
	// Synchronize on the envelope so a slowly scheduled submitter cannot turn
	// this into the pre-publication refusal instead.
	waitFor(t, "the envelope to publish", func() bool {
		_, err := os.Stat(filepath.Join(requestDir(dir), "stale-2.json"))
		return err == nil
	})
	writeControllerMetadata(t, dir, "run-b", newControllerCapabilities())
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a session change must end the wait with an error")
		}
		var stale *staleSessionError
		if !errors.As(err, &stale) || !stale.raced {
			t.Fatalf("error is not the race outcome: %v", err)
		}
		if !strings.Contains(err.Error(), "run-a") || !strings.Contains(err.Error(), "run-b") {
			t.Fatalf("error %q names neither session", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("the race report waited %s, nearly the full timeout", elapsed)
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the wait ignored the session change")
	}
}

// A controller that answers just as it exits must not be reported as a race:
// the acknowledgement, once written, is the outcome.
func TestSubmitRaceReportYieldsToAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	writeControllerMetadata(t, dir, "run-a", newControllerCapabilities())
	errCh := make(chan error, 1)
	flip := make(chan error, 1)
	go func() {
		_, err := submitSettingsRequest(dir, SettingsRequest{
			Version: SettingsRequestVersion, ID: "stale-3", Type: RequestTypeSettings, Session: "run-a",
		}, 10*time.Second)
		errCh <- err
	}()
	// The controller applies the request and a replacement takes over in the
	// same instant the submitter is between its two reads. Synchronize on the
	// envelope so the flip cannot precede publication.
	waitFor(t, "the envelope to publish", func() bool {
		_, err := os.Stat(filepath.Join(requestDir(dir), "stale-3.json"))
		return err == nil
	})
	go func() {
		ackErr := writeJSON(filepath.Join(ackDir(dir), "stale-3.json"), SettingsAck{
			Version: SettingsRequestVersion, ID: "stale-3", Status: AckApplied, Session: "run-a",
		})
		flip <- errors.Join(ackErr, writeControllerMetadataFile(dir, "run-b", newControllerCapabilities()))
	}()
	// Join the flip goroutine on every exit path, the timeout branch
	// included: its writes can land after the submitter answered, and a
	// write error reported through t.Fatal there fires after the test
	// completed and panics the whole suite under load (T111).
	defer func() {
		select {
		case err := <-flip:
			if err != nil {
				t.Errorf("the controller flip failed: %v", err)
			}
		case <-time.After(testBudget(10 * time.Second)):
			t.Error("the controller flip goroutine never finished")
		}
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("an acknowledged request must not be reported as a race: %v", err)
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the wait never finished")
	}
}

// Claim-time validation: a request of a type this controller never claims
// receives the specific incompatibility acknowledgement instead of waiting
// forever. Supported types the caller did not ask for stay untouched.
func TestClaimRejectsUnsupportedRequestType(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(requestDir(dir), "cancel-1.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "cancel-1", Type: "cancel-task", Session: "run-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(requestDir(dir), "keep-import.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "keep-import", Type: RequestTypePlanImport, Session: "run-a",
	}); err != nil {
		t.Fatal(err)
	}
	req, _, err := claimNextRequest(dir, RequestTypeSettings)
	if err != nil {
		t.Fatal(err)
	}
	if req != nil {
		t.Fatalf("a settings claim must not adopt an unrelated type, got %q", req.Type)
	}
	ack, ok := readRequestAck(dir, "cancel-1")
	if !ok {
		t.Fatal("the unsupported request received no acknowledgement")
	}
	if ack.Status != AckError {
		t.Fatalf("ack status %q", ack.Status)
	}
	for _, want := range []string{"unsupported request type", "cancel-task", PublicRequestAddTask, capabilitiesPolicy} {
		if !strings.Contains(ack.Message, want) {
			t.Fatalf("ack %q does not contain %q", ack.Message, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), "cancel-1.json")); statErr == nil {
		t.Fatal("the unsupported envelope must be removed, not left as a dead letter")
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), "keep-import.json")); statErr != nil {
		t.Fatal("a supported type the caller did not claim must survive")
	}
}

// The happy path through the real protocol: a live controller with published
// capabilities still accepts every advertised session-bound type.
func TestSubmitRoundTripWithPublishedCapabilities(t *testing.T) {
	e := protocolEngine(t)
	writeControllerMetadata(t, e.Dir, e.controller.Session, newControllerCapabilities())
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, ReviewEvery: intPtr(5)})
	digest := profileDigest(e.Config.ProfilePath)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			e.processSettingsRequests()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	ack, err := submitSettingsRequest(e.Dir, SettingsRequest{
		Version: SettingsRequestVersion, Session: e.controller.Session,
		ProfileDigest: digest, Fields: map[string]json.RawMessage{"agent": json.RawMessage(`"claude"`)},
	}, 5*time.Second)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
}

func TestCapabilitiesDocumentMatchesSchema(t *testing.T) {
	schema := capabilitiesSchema(t)
	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Todo: "to-do.json"}
	base, runtimeDir := projectPaths(c)
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	// Without a controller the controller section is omitted entirely.
	doc := capabilitiesDocumentFor(c)
	if doc.Controller != nil {
		t.Fatal("no controller may be invented")
	}
	validateCapabilitiesDocument(t, schema, doc)

	// A live controller with capabilities reports them.
	writeControllerMetadata(t, runtimeDir, "run-live", newControllerCapabilities())
	doc = capabilitiesDocumentFor(c)
	if doc.Controller == nil || doc.Controller.AbsentMetadata {
		t.Fatal("published capabilities must be reported")
	}
	if strings.Join(doc.Controller.RequestTypes, ",") != strings.Join(controllerRequestTypes(), ",") {
		t.Fatalf("request types %v", doc.Controller.RequestTypes)
	}
	if doc.Controller.Limits["pending_requests"] != maxPendingRequests {
		t.Fatalf("limits %v", doc.Controller.Limits)
	}
	validateCapabilitiesDocument(t, schema, doc)

	// An older controller without the record stays explicit about it.
	writeControllerMetadata(t, runtimeDir, "run-old", nil)
	doc = capabilitiesDocumentFor(c)
	if doc.Controller == nil || !doc.Controller.AbsentMetadata {
		t.Fatal("absent metadata must be explicit")
	}
	if joinInts(doc.Controller.ContractVersions) != "1" {
		t.Fatalf("derived contract versions %v", doc.Controller.ContractVersions)
	}
	validateCapabilitiesDocument(t, schema, doc)
}

func validateCapabilitiesDocument(t *testing.T, schema []byte, doc *capabilitiesDocument) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(schema, data); err != nil {
		t.Fatalf("the produced capabilities document violates its schema: %v\n%s", err, data)
	}
}

// The plain version 2 summary reports a client/controller build difference
// and states the controller keeps its own build. Metadata without a build
// proves nothing and must not claim a difference.
func TestVersionSummaryNotesControllerBuildDifference(t *testing.T) {
	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Todo: "to-do.json"}
	_, runtimeDir := projectPaths(c)
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}

	writeControllerMetadata(t, runtimeDir, "run-old", newControllerCapabilities())
	var out strings.Builder
	printCapabilitiesSummary(c, &out)
	for _, want := range []string{"Controller: session run-old", "Builds differ", "keeps its original build"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the summary lacks %q:\n%s", want, out.String())
		}
	}

	// A controller record that carries no build cannot support the claim.
	info := writeControllerMetadata(t, runtimeDir, "run-older", nil)
	info.Build = nil
	if err := writeJSON(controllerPath(runtimeDir), info); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printCapabilitiesSummary(c, &out)
	if strings.Contains(out.String(), "Builds differ") {
		t.Fatalf("a missing controller build must not report a difference:\n%s", out.String())
	}
}

func TestVersionCommandContractSelection(t *testing.T) {
	c, err := Parse([]string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Contract != ContractVersionLegacy {
		t.Fatalf("default contract %d", c.Contract)
	}
	c, err = Parse([]string{"version", "--contract", "v2"})
	if err != nil || c.Contract != ContractVersionV2 {
		t.Fatalf("flag selection failed: %v %d", err, c.Contract)
	}
	c, err = Parse([]string{"--version", "--json", "--contract", "legacy"})
	if err != nil || c.Command != "version" || c.Contract != ContractVersionLegacy {
		t.Fatalf("the --version form must parse like the version command: %v", err)
	}
	t.Setenv("LOOPER_CONTRACT", "2")
	if c, err = Parse([]string{"version"}); err != nil || c.Contract != ContractVersionV2 {
		t.Fatalf("environment selection failed: %v %d", err, c.Contract)
	}
	if c, err = Parse([]string{"version", "--contract", "legacy"}); err != nil || c.Contract != ContractVersionLegacy {
		t.Fatalf("the flag must win over the environment: %v %d", err, c.Contract)
	}
	if _, err = Parse([]string{"version", "--contract", "v3"}); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("unknown version must be a usage error naming the range: %v", err)
	}
	// Since AX-014 every command negotiates the version 2 error contract, so
	// version selection parses everywhere. A command whose version 2 success
	// document has not shipped keeps its legacy success document; the
	// migration guide records that transition.
	if c, err = Parse([]string{"status", "--json", "--contract", "v2"}); err != nil || c.Command != "status" || c.Contract != ContractVersionV2 {
		t.Fatalf("version 2 must negotiate the error contract for every command: %v", err)
	}
	// Help stays usable inside a version 2 session.
	if c, err = Parse([]string{"--help"}); err != nil || c.Command != "help" {
		t.Fatalf("help must stay usable: %v", err)
	}
}

func TestVersionCommandOutput(t *testing.T) {
	// The legacy form stays byte-compatible with the frozen document.
	var out strings.Builder
	if code := Main([]string{"version", "--json"}, &out, &strings.Builder{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	schema, err := os.ReadFile(filepath.Join(legacyContractDocs, "schemas", "legacy", "version.schema.json"))
	if err != nil {
		t.Skipf("legacy version schema is absent: %v", err)
	}
	if err = contractschema.ValidateJSON(schema, []byte(out.String())); err != nil {
		t.Fatalf("legacy version output changed shape: %v", err)
	}

	// The version 2 form emits the capabilities document.
	out.Reset()
	if code := Main([]string{"version", "--contract", "v2", "--json"}, &out, &strings.Builder{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if err = json.Unmarshal([]byte(out.String()), &capabilitiesDocument{}); err != nil {
		t.Fatalf("version 2 output is not one capabilities document: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "\n{") {
		t.Fatal("the finite document must be one JSON value")
	}

	// The plain version 2 form renders the same facts as text.
	out.Reset()
	if code := Main([]string{"version", "--contract", "v2"}, &out, &strings.Builder{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	text := out.String()
	for _, want := range []string{"Contract versions: 1, 2", "Controller:", capabilitiesPolicy} {
		if !strings.Contains(text, want) {
			t.Fatalf("plain capabilities summary lacks %q:\n%s", want, text)
		}
	}
}

func TestHelpListsBothVersionForms(t *testing.T) {
	var out strings.Builder
	if code := Main([]string{"--help"}, &out, &strings.Builder{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	text := out.String()
	if !strings.Contains(text, "looper version ") {
		t.Fatal("help must list the version command form")
	}
	if !strings.Contains(text, "--version") {
		t.Fatal("help must list the --version flag form")
	}
	if !strings.Contains(text, "--contract") {
		t.Fatal("help must document the contract selection flag")
	}
}

// The command list the capabilities document reports, the command registry,
// and the parser must stay the same list.
func TestClientCommandsMatchRegistryAndParser(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(legacyContractDocs, "commands.json"))
	if err != nil {
		t.Skipf("command registry is absent: %v", err)
	}
	var registry struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err = json.Unmarshal(b, &registry); err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, command := range registry.Commands {
		registered[command.Name] = true
	}
	source, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	parsed := map[string]bool{}
	for _, name := range parserCommandNames(string(source)) {
		parsed[name] = true
	}
	seen := map[string]bool{}
	for _, name := range clientCommands {
		if seen[name] {
			t.Fatalf("client command %q listed twice", name)
		}
		seen[name] = true
		if !registered[name] {
			t.Errorf("capabilities document lists %q, which commands.json does not", name)
		}
	}
	for name := range registered {
		if !seen[name] {
			t.Errorf("commands.json lists %q, which the capabilities document omits", name)
		}
	}
	for name := range parsed {
		if !seen[name] {
			t.Errorf("the parser accepts %q, which the capabilities document omits", name)
		}
	}
}

// The stop path refuses before publication when the live controller
// advertises that it never answers stop requests.
func TestRunStopRefusesUnadvertisedStop(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	// Replace the live record with one an older build could have written:
	// the repository lock stays held by the same controller session.
	older := writeControllerMetadata(t, e.Dir, e.controller.Session, &ControllerCapabilities{
		ContractVersions: []int{1},
		RequestTypes:     []string{PublicRequestAddTask, PublicRequestAddPlan, PublicRequestSettings, PublicRequestRequeue},
	})
	var out, stderr strings.Builder
	code := runStop(c, e.Dir, &out, &stderr)
	if code == 0 {
		t.Fatal("refusal must exit nonzero")
	}
	if !strings.Contains(stderr.String(), "does not accept") || !strings.Contains(stderr.String(), older.Session) {
		t.Fatalf("stderr must carry the incompatibility result: %s", stderr.String())
	}
	if _, statErr := os.Stat(stopRequestPath(e.Dir)); statErr == nil {
		t.Fatal("no stop request may be written")
	}
}

func mustFirstEntry(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no entries in %s", dir)
	}
	return entries[0].Name()
}

func intPtr(v int) *int { return &v }

// The full add-task command path against a repository an older controller
// owns: the refusal happens before any envelope is queued.
func TestRunAddTaskRefusesUnsupportedRequest(t *testing.T) {
	c := repoConfig(t, "add-task")
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	older := writeControllerMetadata(t, dir, "run-old", &ControllerCapabilities{
		ContractVersions: []int{1},
		RequestTypes:     []string{PublicRequestAddPlan, PublicRequestSettings, PublicRequestRequeue, PublicRequestStop},
	})
	common := filepath.Join(c.Root, ".git")
	f, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }()
	tasks := filepath.Join(c.Root, "tasks.json")
	if err = os.WriteFile(tasks, []byte(`{"id":"T9","title":"nine","priority":1,"status":"todo"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cc := c
	cc.Plans = []string{tasks}
	var out, stderr strings.Builder
	code := runAddTask(cc, &out, &stderr)
	if code != 1 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not accept") || !strings.Contains(stderr.String(), older.Session) {
		t.Fatalf("stderr must carry the incompatibility result: %s", stderr.String())
	}
	entries, _ := os.ReadDir(requestDir(dir))
	if len(entries) > 0 {
		t.Fatalf("no envelope may be queued, found %d", len(entries))
	}
}
