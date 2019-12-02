// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package batcheval

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/pkg/errors"
)

func init() {
	RegisterCommand(roachpb.RefreshRange, DefaultDeclareKeys, RefreshRange)
}

// RefreshRange checks whether the key range specified has any values written in
// the interval [args.RefreshFrom, header.Timestamp].
func RefreshRange(
	ctx context.Context, batch engine.ReadWriter, cArgs CommandArgs, resp roachpb.Response,
) (result.Result, error) {
	args := cArgs.Args.(*roachpb.RefreshRangeRequest)
	h := cArgs.Header

	if h.Txn == nil {
		return result.Result{}, errors.Errorf("no transaction specified to %s", args.Method())
	}

	// We're going to refresh up to the transaction's read timestamp.
	if h.Timestamp != h.Txn.WriteTimestamp {
		// We're expecting the read and write timestamp to have converged before the
		// Refresh request was sent.
		log.Fatalf(ctx, "expected provisional commit ts %s == read ts %s. txn: %s", h.Timestamp,
			h.Txn.WriteTimestamp, h.Txn)
	}
	refreshTo := h.Timestamp

	refreshFrom := args.RefreshFrom
	if refreshFrom.IsEmpty() {
		// Compatibility with 19.2 nodes, which didn't set the args.RefreshFrom field.
		refreshFrom = h.Txn.DeprecatedOrigTimestamp
	}

	// Iterate over values until we discover any value written at or after the
	// original timestamp, but before or at the current timestamp. Note that we
	// iterate inconsistently without using the txn. This reads only committed
	// values and returns all intents, including those from the txn itself. Note
	// that we include tombstones, which must be considered as updates on refresh.
	log.VEventf(ctx, 2, "refresh %s @[%s-%s]", args.Span(), refreshFrom, refreshTo)
	intents, err := engine.MVCCIterate(
		ctx, batch, args.Key, args.EndKey, refreshTo,
		engine.MVCCScanOptions{
			Inconsistent: true,
			Tombstones:   true,
		},
		func(kv roachpb.KeyValue) (bool, error) {
			if ts := kv.Value.Timestamp; !ts.Less(refreshFrom) {
				return true, errors.Errorf("encountered recently written key %s @%s", kv.Key, ts)
			}
			return false, nil
		})
	if err != nil {
		return result.Result{}, err
	}

	// Check if any intents which are not owned by this transaction were written
	// at or beneath the refresh timestamp.
	for _, i := range intents {
		// Ignore our own intents.
		if i.Txn.ID == h.Txn.ID {
			continue
		}
		// Return an error if an intent was written to the span.
		return result.Result{}, errors.Errorf("encountered recently written intent %s @%s",
			i.Span.Key, i.Txn.WriteTimestamp)
	}

	return result.Result{}, nil
}