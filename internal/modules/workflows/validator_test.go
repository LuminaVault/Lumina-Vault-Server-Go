package workflows

import (
	"encoding/json"
	"fmt"
	"lumina/internal/wire"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func testNode(kind WorkflowNodeKind, config StringMap) WorkflowNodeDTO {
	if config == nil {
		config = StringMap{}
	}
	return WorkflowNodeDTO{Id: wire.UUID{UUID: uuid.New()}, Kind: kind, Name: string(kind), Configuration: config}
}

func testEdge(from, to WorkflowNodeDTO) WorkflowEdgeDTO {
	return WorkflowEdgeDTO{Id: wire.UUID{UUID: uuid.New()}, SourceNodeID: from.Id, SourcePort: "output", TargetNodeID: to.Id, TargetPort: "input"}
}

func testDefinition(trigger WorkflowTriggerKind, nodes []WorkflowNodeDTO, edges []WorkflowEdgeDTO) WorkflowDefinitionDTO {
	return WorkflowDefinitionDTO{Trigger: trigger, TriggerConfiguration: StringMap{}, Nodes: nodes, Edges: edges}
}

// WorkflowValidatorTests.

func TestAcceptsReachableDAG(t *testing.T) {
	trigger, output := testNode(Trigger, nil), testNode(Output, nil)
	require.NoError(t, validateDefinition(testDefinition(Manual, []WorkflowNodeDTO{trigger, output}, []WorkflowEdgeDTO{testEdge(trigger, output)})))
}

func TestRejectsCycles(t *testing.T) {
	trigger, first, second := testNode(Trigger, nil), testNode(Template, nil), testNode(Output, nil)
	err := validateDefinition(testDefinition(Manual, []WorkflowNodeDTO{trigger, first, second}, []WorkflowEdgeDTO{
		testEdge(trigger, first), testEdge(first, second), testEdge(second, first),
	}))
	require.Equal(t, errCycle, err)
}

func TestRejectsUnreachableNodes(t *testing.T) {
	err := validateDefinition(testDefinition(Manual, []WorkflowNodeDTO{testNode(Trigger, nil), testNode(Output, nil)}, nil))
	require.Equal(t, errUnreachableNode, err)
}

func TestRejectsLoopNodesInReliableCore(t *testing.T) {
	trigger, loop := testNode(Trigger, nil), testNode(WhileLoop, StringMap{"maxIterations": "21"})
	err := validateDefinition(testDefinition(Manual, []WorkflowNodeDTO{trigger, loop}, []WorkflowEdgeDTO{testEdge(trigger, loop)}))
	require.Equal(t, errUnsupportedKind(WhileLoop), err)
}

func TestRejectsDeferredExecutionShapes(t *testing.T) {
	for _, kind := range []WorkflowNodeKind{Parallel, ForEach} {
		trigger, deferred := testNode(Trigger, nil), testNode(kind, nil)
		err := validateDefinition(testDefinition(Manual, []WorkflowNodeDTO{trigger, deferred}, []WorkflowEdgeDTO{testEdge(trigger, deferred)}))
		require.Equal(t, errUnsupportedKind(kind), err)
	}
}

func TestRejectsUnknownSchemaVersions(t *testing.T) {
	version := 2
	def := testDefinition(Manual, []WorkflowNodeDTO{testNode(Trigger, nil)}, nil)
	def.SchemaVersion = &version
	require.Equal(t, errUnsupportedSchema(2), validateDefinition(def))
}

func TestRejectsOversizedParallelGroups(t *testing.T) {
	trigger := testNode(Trigger, nil)
	nodes := []WorkflowNodeDTO{trigger}
	var edges []WorkflowEdgeDTO
	for range maxParallelNodes + 1 {
		branch := testNode(Template, StringMap{"parallelGroup": "fanout"})
		nodes = append(nodes, branch)
		edges = append(edges, testEdge(trigger, branch))
	}
	require.Equal(t, errParallelGroup, validateDefinition(testDefinition(Manual, nodes, edges)))
}

func TestValidatorOrderAndBounds(t *testing.T) {
	trigger := testNode(Trigger, nil)
	dup := testNode(Output, nil)
	for name, c := range map[string]struct {
		def  WorkflowDefinitionDTO
		want error
	}{
		"no trigger":                          {testDefinition(Manual, nil, nil), errMissingTrigger},
		"duplicates before a missing trigger": {testDefinition(Manual, []WorkflowNodeDTO{dup, dup}, nil), errDuplicateNodeID},
		"edge to nowhere":                     {testDefinition(Manual, []WorkflowNodeDTO{trigger}, []WorkflowEdgeDTO{testEdge(trigger, dup)}), errUnknownEdgeNode},
		"iteration bound before reachability": {testDefinition(Manual, []WorkflowNodeDTO{trigger, testNode(Output, StringMap{"maxIterations": "21"})}, nil), errIterationBound},
		"iterations words":                    {testDefinition(Manual, []WorkflowNodeDTO{trigger, testNode(Output, StringMap{"maxIterations": "abc"})}, nil), errIterationBound},
	} {
		require.Equal(t, c.want, validateDefinition(c.def), name)
	}
	nodes := []WorkflowNodeDTO{trigger}
	var edges []WorkflowEdgeDTO
	for range 5 {
		branch := testNode(Output, StringMap{"maxIterations": "20"})
		nodes, edges = append(nodes, branch), append(edges, testEdge(trigger, branch))
	}
	require.Equal(t, errExpandedExecution, validateDefinition(testDefinition(Manual, nodes, edges)), "1 + 5 x 20 is over 100")
}

// WorkflowTierPolicyTests.

func TestPaidTierCaps(t *testing.T) {
	for _, c := range []struct {
		tier             string
		active, schedule int
		run, day, month  int64
	}{
		{tierPro, 1, 60, 200_000, 500_000, 2_000_000},
		{tierUltimate, 3, 5, 1_000_000, 2_000_000, 8_000_000},
	} {
		p := policyFor(c.tier)
		require.Equal(t, c.active, p.activeRunLimit, c.tier)
		require.Equal(t, c.schedule, p.minimumScheduleMinutes, c.tier)
		require.Equal(t, c.run, p.perRunUsdMicros, c.tier)
		require.Equal(t, c.day, p.dailyUsdMicros, c.tier)
		require.Equal(t, c.month, p.monthlyUsdMicros, c.tier)
	}
}

func TestNonAuthoringTiers(t *testing.T) {
	for _, tier := range []string{"free", "trial", "lapsed", "archived"} {
		p := policyFor(tier)
		require.Zero(t, p.activeRunLimit, tier)
		require.Zero(t, p.perRunUsdMicros, tier)
	}
}

// WorkflowSchedulePolicyTests.

func scheduled(cron string) WorkflowDefinitionDTO {
	def := testDefinition(Schedule, []WorkflowNodeDTO{testNode(Trigger, nil)}, nil)
	def.TriggerConfiguration = StringMap{"cron": cron, "timezone": "UTC"}
	return def
}

func TestProRejectsSubHourlySchedule(t *testing.T) {
	require.Equal(t, errScheduleTooFrequent(60), validateSchedule(scheduled("*/30 * * * *"), 60, time.Now()))
}

func TestUltimateAcceptsFiveMinuteSchedule(t *testing.T) {
	require.NoError(t, validateSchedule(scheduled("*/5 * * * *"), 5, time.Now()))
}

func TestRejectsInvalidCron(t *testing.T) {
	require.Equal(t, errInvalidCron, validateSchedule(scheduled("not-a-cron"), 5, time.Now()))
}

func TestScheduleEdges(t *testing.T) {
	now := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC)
	require.Equal(t, errScheduleTooFrequent(0), validateSchedule(scheduled("0 * * * *"), 0, now), "a tier that can't author")
	require.NoError(t, validateSchedule(testDefinition(Manual, nil, nil), 0, now), "only a schedule trigger is read")
	missing := scheduled("x")
	missing.TriggerConfiguration = StringMap{}
	require.Equal(t, errInvalidCron, validateSchedule(missing, 5, now))
	missing.TriggerConfiguration = StringMap{"runAt": "whenever"}
	require.NoError(t, validateSchedule(missing, 5, now), "runAt is not parsed")
	require.NoError(t, validateSchedule(scheduled("0 * * * *"), 60, now))
	bad := scheduled("0 * * * *")
	bad.TriggerConfiguration["timezone"] = "Mars/Olympus"
	require.NoError(t, validateSchedule(bad, 60, now), "an unknown zone is UTC")
}

func TestCronParse(t *testing.T) {
	for raw, ok := range map[string]bool{
		"* * * * *": true, "  0   7 * * * ": true, "*/5 * * * *": true, "0-30/30 * * * *": true, "/5 * * * *": true, "0,,30 * * * *": true, ", * * * *": true,
		"* * * *": false, "0 0 0 * * *": false, "60 * * * *": false, "0 24 * * *": false, "0 0 0 * *": false, "0 0 * 13 *": false, "0 0 * * 7": false,
		"*/0 * * * *": false, "*/2/3 * * * *": false, "5-1 * * * *": false, "5- * * * *": false, "1-2-3 * * * *": false, "/ * * * *": false, "MON * * * *": false,
	} {
		_, got := parseCron(raw)
		require.Equal(t, ok, got, raw)
	}
}

func TestCronMatches(t *testing.T) {
	at := time.Date(2026, 3, 2, 7, 0, 0, 0, time.UTC) // a Monday
	for raw, want := range map[string]bool{
		"0 7 * * *": true, "1 7 * * *": false, "0 8 * * *": false, "0 7 * 4 *": false, "0 7 2 * *": true, "0 7 * * 1": true, "0 7 * * 2": false,
		"0 7 3 * 1": true, "0 7 2 * 2": true, "0 7 3 * 2": false,
	} {
		c, ok := parseCron(raw)
		require.True(t, ok, raw)
		require.Equal(t, want, c.matches(at, time.UTC), raw)
	}
	c, ok := parseCron("0 16 * * *")
	require.True(t, ok)
	require.True(t, c.matches(at, scheduleZone("Asia/Tokyo")))
	require.Equal(t, time.UTC, scheduleZone("Mars/Olympus"))
	require.Equal(t, time.UTC, scheduleZone(""))
}

// WorkflowWebhookSignatureTests.

func TestAcceptsExactBodyAndTimestamp(t *testing.T) {
	body := []byte(`{"event":"created"}`)
	require.True(t, verifyWebhook(signWebhook("test-secret", "1720000000", body), "test-secret", "1720000000", body))
}

func TestRejectsTamperedPayloadOrTimestamp(t *testing.T) {
	body := []byte(`{"event":"created"}`)
	signature := signWebhook("test-secret", "1720000000", body)
	require.False(t, verifyWebhook(signature, "test-secret", "1720000001", body))
	require.False(t, verifyWebhook(signature, "test-secret", "1720000000", []byte("tampered")))
}

func TestFlattenPayload(t *testing.T) {
	got := flattenPayload([]byte(`{"s":"t","i":7,"f":1.5,"b":true,"c":false,"n":null,"o":{"a":1},"l":[1]}`))
	require.Equal(t, "t", got["s"])
	require.Equal(t, "7", got["i"])
	require.Equal(t, "1.5", got["f"])
	require.Equal(t, "", got["n"])
	require.JSONEq(t, `{"a":1}`, got["o"])
	require.JSONEq(t, `[1]`, got["l"])
	require.Contains(t, got["raw"], `"s":"t"`)
	require.Equal(t, StringMap{"raw": "[1]"}, flattenPayload([]byte(`[1]`)))
	require.Equal(t, StringMap{"raw": `{"x":1e400,"y":"z"}`}, flattenPayload([]byte(`{"x":1e400,"y":"z"}`)), "an overflow fails the whole parse")
	require.Equal(t, `[1,1000,1.5,1e+16]`, flattenPayload([]byte(`{"l":[1.0,1e3,1.5,1e16]}`))["l"], "nested numbers are written as JSON")
	require.Equal(t, StringMap{"raw": "nope"}, flattenPayload([]byte(`nope`)))
	require.Equal(t, StringMap{"raw": "mine"}, flattenPayload([]byte(`{"raw":"mine"}`)))
}

func TestNumberString(t *testing.T) {
	for raw, want := range map[string]string{
		"7": "7", "-1": "-1", "0": "0", "-0": "0", "1e3": "1000.0", "1.5": "1.5", "1.0": "1.0", "0.1": "0.1",
		"1e16": "1e+16", "1e-5": "1e-05", "0.0001": "0.0001", "9007199254740992.0": "9007199254740992", "900719925474099.5": "900719925474099.5",
		"1.5e300": "1.5e+300", "-2.5": "-2.5", "-0.0": "-0.0", "18446744073709551615": "18446744073709551615",
		"18446744073709551616": "18446744073709551616", "-9223372036854775808": "-9223372036854775808",
		"0.12345678901234567890": "0.1234567890123456789", "123456789012345678901234567890": "123456789012345678901234567890",
	} {
		require.Equal(t, want, numberString(json.Number(raw)), raw)
	}
}

func TestEnabled(t *testing.T) {
	for raw, want := range map[string]bool{"": true, "true": true, "no": false, "NO": false, "0": false, "false": false, "False": false, "banana": true, "1": true} {
		require.Equal(t, want, Enabled(raw), fmt.Sprintf("%q", raw))
	}
}
