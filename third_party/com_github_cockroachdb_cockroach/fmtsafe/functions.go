// Copyright 2020 The Cockroach Authors.
// SPDX-License-Identifier: Apache-2.0

package fmtsafe

import (
	"source.monogon.dev/third_party/com_github_cockroachdb_cockroach/errwrap"
)

// requireConstMsg records functions for which the last string
// argument must be a constant string.
var requireConstMsg = map[string]bool{}

// requireConstFmt records functions for which the string arg
// before the final ellipsis must be a constant string.
var requireConstFmt = map[string]bool{
	// Logging things.
	"log.Printf":           true,
	"log.Fatalf":           true,
	"log.Panicf":           true,
	"(*log.Logger).Fatalf": true,
	"(*log.Logger).Panicf": true,
	"(*log.Logger).Printf": true,

	"(go.etcd.io/etcd/raft/v3.Logger).Debugf":   true,
	"(go.etcd.io/etcd/raft/v3.Logger).Infof":    true,
	"(go.etcd.io/etcd/raft/v3.Logger).Warningf": true,
	"(go.etcd.io/etcd/raft/v3.Logger).Errorf":   true,
	"(go.etcd.io/etcd/raft/v3.Logger).Fatalf":   true,
	"(go.etcd.io/etcd/raft/v3.Logger).Panicf":   true,

	"(google.golang.org/grpc/grpclog.Logger).Infof":    true,
	"(google.golang.org/grpc/grpclog.Logger).Warningf": true,
	"(google.golang.org/grpc/grpclog.Logger).Errorf":   true,
}

func init() {
	for errorFn, formatStringIndex := range errwrap.ErrorFnFormatStringIndex {
		if formatStringIndex < 0 {
			requireConstMsg[errorFn] = true
		} else {
			requireConstFmt[errorFn] = true
		}
	}
}
