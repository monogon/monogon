// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// This file contains common implementation related to filtering of protobuf
// messages with Common Expression Language.
package curator

import (
	"context"

	"github.com/google/cel-go/cel"
	celdecls "github.com/google/cel-go/checker/decls"
	"github.com/google/cel-go/common/decls"
	celtypes "github.com/google/cel-go/common/types"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"source.monogon.dev/metropolis/node/core/rpc"
	apb "source.monogon.dev/metropolis/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

// enumDeclarations is a helper function returning a cel.EnvOption
// that contains CEL integer constant declarations matching em. It's
// purpose is to facilitate importing of proto-defined enums into CEL
// environments.
func enumDeclarations(em map[int32]string) cel.EnvOption {
	var ds []*exprpb.Decl
	for i, n := range em {
		ds = append(ds, celdecls.NewConst(n, celdecls.Int,
			&exprpb.Constant{
				ConstantKind: &exprpb.Constant_Int64Value{Int64Value: int64(i)},
			},
		))
	}

	// nolint:SA1019
	return cel.Declarations(ds...)
}

// buildFilter takes the CEL expression fexpr, and CEL environment options
// opts, to produce a filter program. Since its anticipated usage context
// resides in RPC handlers, it returns RPC-safe, sanitized error messages,
// while utilizing rpc.Trace to log relevant details.
func buildFilter(ctx context.Context, fexpr string, opts ...cel.EnvOption) (cel.Program, error) {
	// Create the CEL environment, containing a node the filter is evaluated
	// against, and related enum constants.
	env, err := cel.NewEnv(opts...)
	if err != nil {
		rpc.Trace(ctx).Printf("Couldn't create a CEL environment: %v", err)
		return nil, status.Errorf(codes.Unavailable, "couldn't process the filter expression: %v", err)
	}

	// Parse and type-check the expression.
	p, iss := env.Parse(fexpr)
	if iss != nil && iss.Err() != nil {
		return nil, status.Errorf(codes.InvalidArgument, "while parsing the filter expression: %v", iss.Err())
	}
	c, iss := env.Check(p)
	if iss != nil && iss.Err() != nil {
		return nil, status.Errorf(codes.InvalidArgument, "while checking the filter expression: %v", iss.Err())
	}

	// Create the filter program.
	fprg, err := env.Program(c)
	if err != nil {
		rpc.Trace(ctx).Printf("Couldn't create a CEL filter program: %v", err)
		return nil, status.Errorf(codes.Unavailable, "couldn't create the filter program")
	}
	return fprg, nil
}

// evaluateFilter is a helper function that takes a CEL program fprg along with
// its runtime environment variables varmap, and evaluates it, expecting a
// boolean result. It returns RPC-safe, sanitized error messages, while
// utilizing rpc.Trace to log relevant details.
func evaluateFilter(ctx context.Context, fprg cel.Program, varmap map[string]interface{}) (bool, error) {
	out, _, err := fprg.Eval(varmap)
	if err != nil {
		rpc.Trace(ctx).Printf("Couldn't evaluate a CEL program: %v", err)
		return false, status.Errorf(codes.Unavailable, "couldn't evaluate the filter expression: %v", err)
	}

	res := out.ConvertToType(celtypes.BoolType)
	if celtypes.IsError(res) {
		return false, status.Errorf(codes.InvalidArgument, "filter did not evaluate to a boolean value")
	}
	return res == celtypes.True, nil
}

// nodeFilter are functions created by buildNodeFilter, corresponding to a
// specific CEL filter expression, that wrap evaluateFilter.
type nodeFilter func(ctx context.Context, node *apb.Node) (bool, error)

// buildNodeFilter wraps buildFilter to return a node filtering function based
// on the CEL filter expression expr. Given an empty filter expression, it
// returns a function that keeps every node.
func buildNodeFilter(ctx context.Context, expr string) (nodeFilter, error) {
	if expr == "" {
		return func(_ context.Context, _ *apb.Node) (bool, error) {
			return true, nil
		}, nil
	}

	// Build the filtering CEL program using the expression, along with
	// node-specific CEL environment options.
	fprg, err := buildFilter(ctx, expr,
		cel.Types(&apb.Node{}),
		cel.VariableDecls(decls.NewVariable("node", celtypes.NewTypeParamType("metropolis.proto.api.Node"))),
		// There doesn't seem to be an easier way of importing protobuf enums
		// into CEL environments.
		enumDeclarations(cpb.NodeState_name),
		enumDeclarations(apb.Node_Health_name),
	)
	if err != nil {
		return nil, err
	}

	// Return a filtering function that captures fprg.
	return func(ctx context.Context, n *apb.Node) (bool, error) {
		keep, err := evaluateFilter(ctx, fprg, map[string]interface{}{
			"node": n,
		})
		return keep, err
	}, nil
}
