package nomad

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	log "github.com/hashicorp/go-hclog"
	cstructs "github.com/hashicorp/nomad/client/structs"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/structs"
)

// FileSystem endpoint is used for accessing the logs and filesystem of
// allocations from a Node.
type FileSystem struct {
	srv    *Server
	logger log.Logger
}

func (f *FileSystem) register() {
	f.srv.streamingRpcs.Register("FileSystem.Logs", f.logs)
	f.srv.streamingRpcs.Register("FileSystem.Stream", f.stream)
}

// handleStreamResultError is a helper for sending an error with a potential
// error code. The transmission of the error is ignored if the error has been
// generated by the closing of the underlying transport.
func handleStreamResultError(err error, code *int64, encoder *codec.Encoder) {
	// Nothing to do as the conn is closed
	if err == io.EOF || strings.Contains(err.Error(), "closed") {
		return
	}

	// Attempt to send the error
	encoder.Encode(&cstructs.StreamErrWrapper{
		Error: cstructs.NewRpcError(err, code),
	})
}

// forwardRegionStreamingRpc is used to make a streaming RPC to a different
// region. It looks up the allocation in the remote region to determine what
// remote server can route the request.
func forwardRegionStreamingRpc(fsrv *Server, conn io.ReadWriteCloser,
	encoder *codec.Encoder, args interface{}, method, allocID string, qo *structs.QueryOptions) {
	// Request the allocation from the target region
	allocReq := &structs.AllocSpecificRequest{
		AllocID:      allocID,
		QueryOptions: *qo,
	}
	var allocResp structs.SingleAllocResponse
	if err := fsrv.forwardRegion(qo.RequestRegion(), "Alloc.GetAlloc", allocReq, &allocResp); err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	if allocResp.Alloc == nil {
		handleStreamResultError(structs.NewErrUnknownAllocation(allocID), helper.Int64ToPtr(404), encoder)
		return
	}

	// Determine the Server that has a connection to the node.
	srv, err := fsrv.serverWithNodeConn(allocResp.Alloc.NodeID, qo.RequestRegion())
	if err != nil {
		var code *int64
		if structs.IsErrNoNodeConn(err) {
			code = helper.Int64ToPtr(404)
		}
		handleStreamResultError(err, code, encoder)
		return
	}

	// Get a connection to the server
	srvConn, err := fsrv.streamingRpc(srv, method)
	if err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}
	defer srvConn.Close()

	// Send the request.
	outEncoder := codec.NewEncoder(srvConn, structs.MsgpackHandle)
	if err := outEncoder.Encode(args); err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	structs.Bridge(conn, srvConn)
}

// List is used to list the contents of an allocation's directory.
func (f *FileSystem) List(args *cstructs.FsListRequest, reply *cstructs.FsListResponse) error {
	// We only allow stale reads since the only potentially stale information is
	// the Node registration and the cost is fairly high for adding another hope
	// in the forwarding chain.
	args.QueryOptions.AllowStale = true

	// Potentially forward to a different region.
	if done, err := f.srv.forward("FileSystem.List", args, args, reply); done {
		return err
	}
	defer metrics.MeasureSince([]string{"nomad", "file_system", "list"}, time.Now())

	// Verify the arguments.
	if args.AllocID == "" {
		return errors.New("missing allocation ID")
	}

	// Lookup the allocation
	snap, err := f.srv.State().Snapshot()
	if err != nil {
		return err
	}

	alloc, err := getAlloc(snap, args.AllocID)
	if err != nil {
		return err
	}

	// Check namespace filesystem read permissions
	allowNsOp := acl.NamespaceValidator(acl.NamespaceCapabilityReadFS)
	aclObj, err := f.srv.ResolveToken(args.AuthToken)
	if err != nil {
		return err
	} else if !allowNsOp(aclObj, alloc.Namespace) {
		return structs.ErrPermissionDenied
	}

	// Make sure Node is valid and new enough to support RPC
	_, err = getNodeForRpc(snap, alloc.NodeID)
	if err != nil {
		return err
	}

	// Get the connection to the client
	state, ok := f.srv.getNodeConn(alloc.NodeID)
	if !ok {
		return findNodeConnAndForward(f.srv, alloc.NodeID, "FileSystem.List", args, reply)
	}

	// Make the RPC
	return NodeRpc(state.Session, "FileSystem.List", args, reply)
}

// Stat is used to stat a file in the allocation's directory.
func (f *FileSystem) Stat(args *cstructs.FsStatRequest, reply *cstructs.FsStatResponse) error {
	// We only allow stale reads since the only potentially stale information is
	// the Node registration and the cost is fairly high for adding another hope
	// in the forwarding chain.
	args.QueryOptions.AllowStale = true

	// Potentially forward to a different region.
	if done, err := f.srv.forward("FileSystem.Stat", args, args, reply); done {
		return err
	}
	defer metrics.MeasureSince([]string{"nomad", "file_system", "stat"}, time.Now())

	// Verify the arguments.
	if args.AllocID == "" {
		return errors.New("missing allocation ID")
	}

	// Lookup the allocation
	snap, err := f.srv.State().Snapshot()
	if err != nil {
		return err
	}

	alloc, err := getAlloc(snap, args.AllocID)
	if err != nil {
		return err
	}

	// Check filesystem read permissions
	if aclObj, err := f.srv.ResolveToken(args.AuthToken); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowNsOp(alloc.Namespace, acl.NamespaceCapabilityReadFS) {
		return structs.ErrPermissionDenied
	}

	// Make sure Node is valid and new enough to support RPC
	_, err = getNodeForRpc(snap, alloc.NodeID)
	if err != nil {
		return err
	}

	// Get the connection to the client
	state, ok := f.srv.getNodeConn(alloc.NodeID)
	if !ok {
		return findNodeConnAndForward(f.srv, alloc.NodeID, "FileSystem.Stat", args, reply)
	}

	// Make the RPC
	return NodeRpc(state.Session, "FileSystem.Stat", args, reply)
}

// stream is is used to stream the contents of file in an allocation's
// directory.
func (f *FileSystem) stream(conn io.ReadWriteCloser) {
	defer conn.Close()
	defer metrics.MeasureSince([]string{"nomad", "file_system", "stream"}, time.Now())

	// Decode the arguments
	var args cstructs.FsStreamRequest
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	encoder := codec.NewEncoder(conn, structs.MsgpackHandle)

	if err := decoder.Decode(&args); err != nil {
		handleStreamResultError(err, helper.Int64ToPtr(500), encoder)
		return
	}

	// Check if we need to forward to a different region
	if r := args.RequestRegion(); r != f.srv.Region() {
		forwardRegionStreamingRpc(f.srv, conn, encoder, &args, "FileSystem.Stream",
			args.AllocID, &args.QueryOptions)
		return
	}

	// Verify the arguments.
	if args.AllocID == "" {
		handleStreamResultError(errors.New("missing AllocID"), helper.Int64ToPtr(400), encoder)
		return
	}

	// Retrieve the allocation
	snap, err := f.srv.State().Snapshot()
	if err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	alloc, err := getAlloc(snap, args.AllocID)
	if structs.IsErrUnknownAllocation(err) {
		handleStreamResultError(structs.NewErrUnknownAllocation(args.AllocID), helper.Int64ToPtr(404), encoder)
		return
	}
	if err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	// Check namespace read-fs permissions.
	if aclObj, err := f.srv.ResolveToken(args.AuthToken); err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	} else if aclObj != nil && !aclObj.AllowNsOp(alloc.Namespace, acl.NamespaceCapabilityReadFS) {
		handleStreamResultError(structs.ErrPermissionDenied, nil, encoder)
		return
	}

	nodeID := alloc.NodeID

	// Make sure Node is valid and new enough to support RPC
	node, err := snap.NodeByID(nil, nodeID)
	if err != nil {
		handleStreamResultError(err, helper.Int64ToPtr(500), encoder)
		return
	}

	if node == nil {
		err := fmt.Errorf("Unknown node %q", nodeID)
		handleStreamResultError(err, helper.Int64ToPtr(400), encoder)
		return
	}

	if err := nodeSupportsRpc(node); err != nil {
		handleStreamResultError(err, helper.Int64ToPtr(400), encoder)
		return
	}

	// Get the connection to the client either by forwarding to another server
	// or creating a direct stream
	var clientConn net.Conn
	state, ok := f.srv.getNodeConn(nodeID)
	if !ok {
		// Determine the Server that has a connection to the node.
		srv, err := f.srv.serverWithNodeConn(nodeID, f.srv.Region())
		if err != nil {
			var code *int64
			if structs.IsErrNoNodeConn(err) {
				code = helper.Int64ToPtr(404)
			}
			handleStreamResultError(err, code, encoder)
			return
		}

		// Get a connection to the server
		conn, err := f.srv.streamingRpc(srv, "FileSystem.Stream")
		if err != nil {
			handleStreamResultError(err, nil, encoder)
			return
		}

		clientConn = conn
	} else {
		stream, err := NodeStreamingRpc(state.Session, "FileSystem.Stream")
		if err != nil {
			handleStreamResultError(err, nil, encoder)
			return
		}
		clientConn = stream
	}
	defer clientConn.Close()

	// Send the request.
	outEncoder := codec.NewEncoder(clientConn, structs.MsgpackHandle)
	if err := outEncoder.Encode(args); err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	structs.Bridge(conn, clientConn)
}

// logs is used to access an task's logs for a given allocation
func (f *FileSystem) logs(conn io.ReadWriteCloser) {
	defer conn.Close()
	defer metrics.MeasureSince([]string{"nomad", "file_system", "logs"}, time.Now())

	// Decode the arguments
	var args cstructs.FsLogsRequest
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	encoder := codec.NewEncoder(conn, structs.MsgpackHandle)

	if err := decoder.Decode(&args); err != nil {
		handleStreamResultError(err, helper.Int64ToPtr(500), encoder)
		return
	}

	// Check if we need to forward to a different region
	if r := args.RequestRegion(); r != f.srv.Region() {
		forwardRegionStreamingRpc(f.srv, conn, encoder, &args, "FileSystem.Logs",
			args.AllocID, &args.QueryOptions)
		return
	}

	// Verify the arguments.
	if args.AllocID == "" {
		handleStreamResultError(structs.ErrMissingAllocID, helper.Int64ToPtr(400), encoder)
		return
	}

	// Retrieve the allocation
	snap, err := f.srv.State().Snapshot()
	if err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	alloc, err := getAlloc(snap, args.AllocID)
	if structs.IsErrUnknownAllocation(err) {
		handleStreamResultError(structs.NewErrUnknownAllocation(args.AllocID), helper.Int64ToPtr(404), encoder)
		return
	}
	if err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	// Check namespace read-logs *or* read-fs permissions.
	allowNsOp := acl.NamespaceValidator(
		acl.NamespaceCapabilityReadFS, acl.NamespaceCapabilityReadLogs)
	aclObj, err := f.srv.ResolveToken(args.AuthToken)
	if err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	} else if !allowNsOp(aclObj, alloc.Namespace) {
		handleStreamResultError(structs.ErrPermissionDenied, nil, encoder)
		return
	}

	nodeID := alloc.NodeID

	// Make sure Node is valid and new enough to support RPC
	node, err := snap.NodeByID(nil, nodeID)
	if err != nil {
		handleStreamResultError(err, helper.Int64ToPtr(500), encoder)
		return
	}

	if node == nil {
		err := fmt.Errorf("Unknown node %q", nodeID)
		handleStreamResultError(err, helper.Int64ToPtr(400), encoder)
		return
	}

	if err := nodeSupportsRpc(node); err != nil {
		handleStreamResultError(err, helper.Int64ToPtr(400), encoder)
		return
	}

	// Get the connection to the client either by forwarding to another server
	// or creating a direct stream
	var clientConn net.Conn
	state, ok := f.srv.getNodeConn(nodeID)
	if !ok {
		// Determine the Server that has a connection to the node.
		srv, err := f.srv.serverWithNodeConn(nodeID, f.srv.Region())
		if err != nil {
			var code *int64
			if structs.IsErrNoNodeConn(err) {
				code = helper.Int64ToPtr(404)
			}
			handleStreamResultError(err, code, encoder)
			return
		}

		// Get a connection to the server
		conn, err := f.srv.streamingRpc(srv, "FileSystem.Logs")
		if err != nil {
			handleStreamResultError(err, nil, encoder)
			return
		}

		clientConn = conn
	} else {
		stream, err := NodeStreamingRpc(state.Session, "FileSystem.Logs")
		if err != nil {
			handleStreamResultError(err, nil, encoder)
			return
		}
		clientConn = stream
	}
	defer clientConn.Close()

	// Send the request.
	outEncoder := codec.NewEncoder(clientConn, structs.MsgpackHandle)
	if err := outEncoder.Encode(args); err != nil {
		handleStreamResultError(err, nil, encoder)
		return
	}

	structs.Bridge(conn, clientConn)
}
