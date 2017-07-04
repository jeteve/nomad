package nomad

import (
	"reflect"
	"testing"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/hashicorp/raft"
)

const (
	// workerPoolSize is the size of the worker pool
	workerPoolSize = 2
)

// planWaitFuture is used to wait for the Raft future to complete
func planWaitFuture(future raft.ApplyFuture) (uint64, error) {
	if err := future.Error(); err != nil {
		return 0, err
	}
	return future.Index(), nil
}

func testRegisterNode(t *testing.T, s *Server, n *structs.Node) {
	// Create the register request
	req := &structs.NodeRegisterRequest{
		Node:         n,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}

	// Fetch the response
	var resp structs.NodeUpdateResponse
	if err := s.RPC("Node.Register", req, &resp); err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Index == 0 {
		t.Fatalf("bad index: %d", resp.Index)
	}
}

func testRegisterJob(t *testing.T, s *Server, j *structs.Job) {
	// Create the register request
	req := &structs.JobRegisterRequest{
		Job:          j,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}

	// Fetch the response
	var resp structs.JobRegisterResponse
	if err := s.RPC("Job.Register", req, &resp); err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Index == 0 {
		t.Fatalf("bad index: %d", resp.Index)
	}
}

func TestPlanApply_applyPlan(t *testing.T) {
	s1 := testServer(t, nil)
	defer s1.Shutdown()
	testutil.WaitForLeader(t, s1.RPC)

	// Register ndoe
	node := mock.Node()
	testRegisterNode(t, s1, node)

	// Register alloc
	alloc := mock.Alloc()
	s1.State().UpsertJobSummary(1000, mock.JobSummary(alloc.JobID))
	planRes := &structs.PlanResult{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc},
		},
	}

	// Snapshot the state
	snap, err := s1.State().Snapshot()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Create the plan with a deployment
	plan := &structs.Plan{
		Job:        alloc.Job,
		Deployment: mock.Deployment(),
	}

	// Apply the plan
	future, err := s1.applyPlan(plan, planRes, snap)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify our optimistic snapshot is updated
	ws := memdb.NewWatchSet()
	if out, err := snap.AllocByID(ws, alloc.ID); err != nil || out == nil {
		t.Fatalf("bad: %v %v", out, err)
	}

	if out, err := snap.DeploymentByID(ws, plan.Deployment.ID); err != nil || out == nil {
		t.Fatalf("bad: %v %v", out, err)
	}

	// Check plan does apply cleanly
	index, err := planWaitFuture(future)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if index == 0 {
		t.Fatalf("bad: %d", index)
	}

	// Lookup the allocation
	fsmState := s1.fsm.State()
	out, err := fsmState.AllocByID(ws, alloc.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out == nil {
		t.Fatalf("missing alloc")
	}

	// Lookup the deployment
	dout, err := fsmState.DeploymentByID(ws, plan.Deployment.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dout == nil {
		t.Fatalf("missing deployment")
	}

	// Evict alloc, Register alloc2
	allocEvict := new(structs.Allocation)
	*allocEvict = *alloc
	allocEvict.DesiredStatus = structs.AllocDesiredStatusEvict
	job := allocEvict.Job
	allocEvict.Job = nil
	alloc2 := mock.Alloc()
	s1.State().UpsertJobSummary(1500, mock.JobSummary(alloc2.JobID))
	planRes = &structs.PlanResult{
		NodeUpdate: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{allocEvict},
		},
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc2},
		},
	}

	// Snapshot the state
	snap, err = s1.State().Snapshot()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Apply the plan
	plan = &structs.Plan{
		Job: job,
	}
	future, err = s1.applyPlan(plan, planRes, snap)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check that our optimistic view is updated
	if out, _ := snap.AllocByID(ws, allocEvict.ID); out.DesiredStatus != structs.AllocDesiredStatusEvict {
		t.Fatalf("bad: %#v", out)
	}

	// Verify plan applies cleanly
	index, err = planWaitFuture(future)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if index == 0 {
		t.Fatalf("bad: %d", index)
	}

	// Lookup the allocation
	out, err = s1.fsm.State().AllocByID(ws, alloc.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.DesiredStatus != structs.AllocDesiredStatusEvict {
		t.Fatalf("should be evicted alloc: %#v", out)
	}
	if out.Job == nil {
		t.Fatalf("missing job")
	}

	// Lookup the allocation
	out, err = s1.fsm.State().AllocByID(ws, alloc2.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out == nil {
		t.Fatalf("missing alloc")
	}
	if out.Job == nil {
		t.Fatalf("missing job")
	}
}

func TestPlanApply_EvalPlan_Simple(t *testing.T) {
	state := testStateStore(t)
	node := mock.Node()
	state.UpsertNode(1000, node)
	snap, _ := state.Snapshot()

	alloc := mock.Alloc()
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc},
		},
	}

	pool := NewEvaluatePool(workerPoolSize, workerPoolBufferSize)
	defer pool.Shutdown()

	result, err := evaluatePlan(pool, snap, plan)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if result == nil {
		t.Fatalf("missing result")
	}
	if !reflect.DeepEqual(result.NodeAllocation, plan.NodeAllocation) {
		t.Fatalf("incorrect node allocations")
	}
}

func TestPlanApply_EvalPlan_Partial(t *testing.T) {
	state := testStateStore(t)
	node := mock.Node()
	state.UpsertNode(1000, node)
	node2 := mock.Node()
	state.UpsertNode(1001, node2)
	snap, _ := state.Snapshot()

	alloc := mock.Alloc()
	alloc2 := mock.Alloc() // Ensure alloc2 does not fit
	alloc2.Resources = node2.Resources
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID:  []*structs.Allocation{alloc},
			node2.ID: []*structs.Allocation{alloc2},
		},
	}

	pool := NewEvaluatePool(workerPoolSize, workerPoolBufferSize)
	defer pool.Shutdown()

	result, err := evaluatePlan(pool, snap, plan)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if result == nil {
		t.Fatalf("missing result")
	}

	if _, ok := result.NodeAllocation[node.ID]; !ok {
		t.Fatalf("should allow alloc")
	}
	if _, ok := result.NodeAllocation[node2.ID]; ok {
		t.Fatalf("should not allow alloc2")
	}
	if result.RefreshIndex != 1001 {
		t.Fatalf("bad: %d", result.RefreshIndex)
	}
}

func TestPlanApply_EvalPlan_Partial_AllAtOnce(t *testing.T) {
	state := testStateStore(t)
	node := mock.Node()
	state.UpsertNode(1000, node)
	node2 := mock.Node()
	state.UpsertNode(1001, node2)
	snap, _ := state.Snapshot()

	alloc := mock.Alloc()
	alloc2 := mock.Alloc() // Ensure alloc2 does not fit
	alloc2.Resources = node2.Resources
	plan := &structs.Plan{
		AllAtOnce: true, // Require all to make progress
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID:  []*structs.Allocation{alloc},
			node2.ID: []*structs.Allocation{alloc2},
		},
	}

	pool := NewEvaluatePool(workerPoolSize, workerPoolBufferSize)
	defer pool.Shutdown()

	result, err := evaluatePlan(pool, snap, plan)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if result == nil {
		t.Fatalf("missing result")
	}

	if len(result.NodeAllocation) != 0 {
		t.Fatalf("should not alloc: %v", result.NodeAllocation)
	}
	if result.RefreshIndex != 1001 {
		t.Fatalf("bad: %d", result.RefreshIndex)
	}
}

func TestPlanApply_EvalNodePlan_Simple(t *testing.T) {
	state := testStateStore(t)
	node := mock.Node()
	state.UpsertNode(1000, node)
	snap, _ := state.Snapshot()

	alloc := mock.Alloc()
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_NodeNotReady(t *testing.T) {
	state := testStateStore(t)
	node := mock.Node()
	node.Status = structs.NodeStatusInit
	state.UpsertNode(1000, node)
	snap, _ := state.Snapshot()

	alloc := mock.Alloc()
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_NodeDrain(t *testing.T) {
	state := testStateStore(t)
	node := mock.Node()
	node.Drain = true
	state.UpsertNode(1000, node)
	snap, _ := state.Snapshot()

	alloc := mock.Alloc()
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_NodeNotExist(t *testing.T) {
	state := testStateStore(t)
	snap, _ := state.Snapshot()

	nodeID := "12345678-abcd-efab-cdef-123456789abc"
	alloc := mock.Alloc()
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			nodeID: []*structs.Allocation{alloc},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, nodeID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_NodeFull(t *testing.T) {
	alloc := mock.Alloc()
	state := testStateStore(t)
	node := mock.Node()
	alloc.NodeID = node.ID
	node.Resources = alloc.Resources
	node.Reserved = nil
	state.UpsertJobSummary(999, mock.JobSummary(alloc.JobID))
	state.UpsertNode(1000, node)
	state.UpsertAllocs(1001, []*structs.Allocation{alloc})

	alloc2 := mock.Alloc()
	alloc2.NodeID = node.ID
	state.UpsertJobSummary(1200, mock.JobSummary(alloc2.JobID))

	snap, _ := state.Snapshot()
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc2},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_UpdateExisting(t *testing.T) {
	alloc := mock.Alloc()
	state := testStateStore(t)
	node := mock.Node()
	alloc.NodeID = node.ID
	node.Resources = alloc.Resources
	node.Reserved = nil
	state.UpsertNode(1000, node)
	state.UpsertAllocs(1001, []*structs.Allocation{alloc})
	snap, _ := state.Snapshot()

	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_NodeFull_Evict(t *testing.T) {
	alloc := mock.Alloc()
	state := testStateStore(t)
	node := mock.Node()
	alloc.NodeID = node.ID
	node.Resources = alloc.Resources
	node.Reserved = nil
	state.UpsertNode(1000, node)
	state.UpsertAllocs(1001, []*structs.Allocation{alloc})
	snap, _ := state.Snapshot()

	allocEvict := new(structs.Allocation)
	*allocEvict = *alloc
	allocEvict.DesiredStatus = structs.AllocDesiredStatusEvict
	alloc2 := mock.Alloc()
	plan := &structs.Plan{
		NodeUpdate: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{allocEvict},
		},
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc2},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_NodeFull_AllocEvict(t *testing.T) {
	alloc := mock.Alloc()
	state := testStateStore(t)
	node := mock.Node()
	alloc.NodeID = node.ID
	alloc.DesiredStatus = structs.AllocDesiredStatusEvict
	node.Resources = alloc.Resources
	node.Reserved = nil
	state.UpsertNode(1000, node)
	state.UpsertAllocs(1001, []*structs.Allocation{alloc})
	snap, _ := state.Snapshot()

	alloc2 := mock.Alloc()
	plan := &structs.Plan{
		NodeAllocation: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{alloc2},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !fit {
		t.Fatalf("bad")
	}
}

func TestPlanApply_EvalNodePlan_NodeDown_EvictOnly(t *testing.T) {
	alloc := mock.Alloc()
	state := testStateStore(t)
	node := mock.Node()
	alloc.NodeID = node.ID
	node.Resources = alloc.Resources
	node.Reserved = nil
	node.Status = structs.NodeStatusDown
	state.UpsertNode(1000, node)
	state.UpsertAllocs(1001, []*structs.Allocation{alloc})
	snap, _ := state.Snapshot()

	allocEvict := new(structs.Allocation)
	*allocEvict = *alloc
	allocEvict.DesiredStatus = structs.AllocDesiredStatusEvict
	plan := &structs.Plan{
		NodeUpdate: map[string][]*structs.Allocation{
			node.ID: []*structs.Allocation{allocEvict},
		},
	}

	fit, err := evaluateNodePlan(snap, plan, node.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !fit {
		t.Fatalf("bad")
	}
}
