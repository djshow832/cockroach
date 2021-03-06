// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storage

import (
	"bytes"
	"context"
	"math"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/kr/pretty"
)

// optimizePuts searches for contiguous runs of Put & CPut commands in
// the supplied request union. Any run which exceeds a minimum length
// threshold employs a full order iterator to determine whether the
// range of keys being written is empty. If so, then the run can be
// set to put "blindly", meaning no iterator need be used to read
// existing values during the MVCC write.
// The caller should use the returned slice (which is either equal to
// the input slice, or has been shallow-copied appropriately to avoid
// mutating the original requests).
func optimizePuts(
	reader engine.Reader, origReqs []roachpb.RequestUnion, distinctSpans bool,
) []roachpb.RequestUnion {
	var minKey, maxKey roachpb.Key
	var unique map[string]struct{}
	if !distinctSpans {
		unique = make(map[string]struct{}, len(origReqs))
	}
	// Returns false on occurrence of a duplicate key.
	maybeAddPut := func(key roachpb.Key) bool {
		// Note that casting the byte slice key to a string does not allocate.
		if unique != nil {
			if _, ok := unique[string(key)]; ok {
				return false
			}
			unique[string(key)] = struct{}{}
		}
		if minKey == nil || bytes.Compare(key, minKey) < 0 {
			minKey = key
		}
		if maxKey == nil || bytes.Compare(key, maxKey) > 0 {
			maxKey = key
		}
		return true
	}

	firstUnoptimizedIndex := len(origReqs)
	for i, r := range origReqs {
		switch t := r.GetInner().(type) {
		case *roachpb.PutRequest:
			if maybeAddPut(t.Key) {
				continue
			}
		case *roachpb.ConditionalPutRequest:
			if maybeAddPut(t.Key) {
				continue
			}
		case *roachpb.InitPutRequest:
			if maybeAddPut(t.Key) {
				continue
			}
		}
		firstUnoptimizedIndex = i
		break
	}

	if firstUnoptimizedIndex < optimizePutThreshold { // don't bother if below this threshold
		return origReqs
	}
	iter := reader.NewIterator(engine.IterOptions{
		// We want to include maxKey in our scan. Since UpperBound is exclusive, we
		// need to set it to the key after maxKey.
		UpperBound: maxKey.Next(),
	})
	defer iter.Close()

	// If there are enough puts in the run to justify calling seek,
	// we can determine whether any part of the range being written
	// is "virgin" and set the puts to write blindly.
	// Find the first non-empty key in the run.
	iter.SeekGE(engine.MakeMVCCMetadataKey(minKey))
	var iterKey roachpb.Key
	if ok, err := iter.Valid(); err != nil {
		// TODO(bdarnell): return an error here instead of silently
		// running without the optimization?
		log.Errorf(context.TODO(), "Seek returned error; disabling blind-put optimization: %+v", err)
		return origReqs
	} else if ok && bytes.Compare(iter.Key().Key, maxKey) <= 0 {
		iterKey = iter.Key().Key
	}
	// Set the prefix of the run which is being written to virgin
	// keyspace to "blindly" put values.
	reqs := append([]roachpb.RequestUnion(nil), origReqs...)
	for i := range reqs[:firstUnoptimizedIndex] {
		inner := reqs[i].GetInner()
		if iterKey == nil || bytes.Compare(iterKey, inner.Header().Key) > 0 {
			switch t := inner.(type) {
			case *roachpb.PutRequest:
				shallow := *t
				shallow.Blind = true
				reqs[i].MustSetInner(&shallow)
			case *roachpb.ConditionalPutRequest:
				shallow := *t
				shallow.Blind = true
				reqs[i].MustSetInner(&shallow)
			case *roachpb.InitPutRequest:
				shallow := *t
				shallow.Blind = true
				reqs[i].MustSetInner(&shallow)
			default:
				log.Fatalf(context.TODO(), "unexpected non-put request: %s", t)
			}
		}
	}
	return reqs
}

// evaluateBatch evaluates a batch request by splitting it up into its
// individual commands, passing them to evaluateCommand, and combining
// the results.
func evaluateBatch(
	ctx context.Context,
	idKey storagebase.CmdIDKey,
	readWriter engine.ReadWriter,
	rec batcheval.EvalContext,
	ms *enginepb.MVCCStats,
	ba *roachpb.BatchRequest,
	readOnly bool,
) (*roachpb.BatchResponse, result.Result, *roachpb.Error) {
	// NB: Don't mutate BatchRequest directly.
	baReqs := ba.Requests
	baHeader := ba.Header
	br := ba.CreateReply()

	maxKeys := int64(math.MaxInt64)
	if baHeader.MaxSpanRequestKeys != 0 {
		// We have a batch of requests with a limit. We keep track of how many
		// remaining keys we can touch.
		maxKeys = baHeader.MaxSpanRequestKeys
	}

	// Optimize any contiguous sequences of put and conditional put ops.
	if len(baReqs) >= optimizePutThreshold && !readOnly {
		baReqs = optimizePuts(readWriter, baReqs, baHeader.DistinctSpans)
	}

	// Create a clone of the transaction to store the new txn state produced on
	// the return/error path.
	if baHeader.Txn != nil {
		baHeader.Txn = baHeader.Txn.Clone()

		// Check whether this transaction has been aborted, if applicable.
		// This applies to writes that leave intents (the use of the
		// IsTransactionWrite flag excludes operations like HeartbeatTxn),
		// and reads that occur in a transaction that has already written
		// (see #2231 for more about why we check for aborted transactions
		// on reads). Note that 1PC transactions have had their
		// transaction field cleared by this point so we do not execute
		// this check in that case.
		if ba.IsTransactionWrite() || baHeader.Txn.IsWriting() {
			// We don't check the abort span for a couple of special requests:
			// - if the request is asking to abort the transaction, then don't check the
			// AbortSpan; we don't want the request to be rejected if the transaction
			// has already been aborted.
			// - heartbeats don't check the abort span. If the txn is aborted, they'll
			// return an aborted proto in their otherwise successful response.
			// TODO(nvanbenschoten): Let's remove heartbeats from this whitelist when
			// we rationalize the TODO in txnHeartbeater.heartbeat.
			if !ba.IsSingleAbortTxnRequest() && !ba.IsSingleHeartbeatTxnRequest() {
				if pErr := checkIfTxnAborted(ctx, rec, readWriter, *baHeader.Txn); pErr != nil {
					return nil, result.Result{}, pErr
				}
			}
		}
	}

	var mergedResult result.Result

	// WriteTooOldErrors have particular handling. When a request encounters the
	// error, we'd like to lay down an intent in order to avoid writers being
	// starved. So, for blind writes, we swallow the error and instead we set the
	// WriteTooOld flag on the response. For non-blind writes (e.g. CPut), we
	// can't do that and so we just return the WriteTooOldError - see note on
	// IsReadAndWrite() stanza below. Upon receiving either a WriteTooOldError or
	// a response with the WriteTooOld flag set, the client will attempt to bump
	// the txn's read timestamp through a refresh. If successful, the client will
	// retry this batch (in both cases).
	//
	// In any case, evaluation of the current batch always continue after a
	// WriteTooOldError in order to find out if there's more conflicts and chose a
	// final write timestamp.
	var writeTooOldState struct {
		err *roachpb.WriteTooOldError
		// cantDeferWTOE is set when a WriteTooOldError cannot be deferred past the
		// end of the current batch.
		cantDeferWTOE bool
	}

	for index, union := range baReqs {
		// Execute the command.
		args := union.GetInner()

		if baHeader.Txn != nil {
			// Set the Request's sequence number on the TxnMeta for this
			// request. The MVCC layer (currently) uses TxnMeta to
			// pass input arguments, such as the seqnum at which a
			// request operates.
			baHeader.Txn.Sequence = args.Header().Sequence
		}

		// Note that responses are populated even when an error is returned.
		// TODO(tschottdorf): Change that. IIRC there is nontrivial use of it currently.
		reply := br.Responses[index].GetInner()

		var curResult result.Result
		var pErr *roachpb.Error
		curResult, pErr = evaluateCommand(
			ctx, idKey, index, readWriter, rec, ms, baHeader, maxKeys, args, reply)

		// If an EndTxn wants to restart because of a write too old, we
		// might have a better error to return to the client.
		retErr, ok := pErr.GetDetail().(*roachpb.TransactionRetryError)
		if ok && retErr.Reason == roachpb.RETRY_WRITE_TOO_OLD &&
			args.Method() == roachpb.EndTxn && writeTooOldState.err != nil {
			pErr.SetDetail(writeTooOldState.err)
			// Don't defer this error. We could perhaps rely on the client observing
			// the WriteTooOld flag and retry the batch, but we choose not too.
			writeTooOldState.cantDeferWTOE = true
		}

		if err := mergedResult.MergeAndDestroy(curResult); err != nil {
			// TODO(tschottdorf): see whether we really need to pass nontrivial
			// Result up on error and if so, formalize that.
			log.Fatalf(
				ctx,
				"unable to absorb Result: %s\ndiff(new, old): %s",
				err, pretty.Diff(curResult, mergedResult),
			)
		}

		if pErr != nil {
			// Initialize the error index.
			pErr.SetErrorIndex(int32(index))

			switch tErr := pErr.GetDetail().(type) {
			case *roachpb.WriteTooOldError:
				// We got a WriteTooOldError. We continue on to run all
				// commands in the batch in order to determine the highest
				// timestamp for more efficient retries. If the batch is
				// transactional, we continue to lay down intents so that
				// other concurrent overlapping transactions are forced
				// through intent resolution and the chances of this batch
				// succeeding when it will be retried are increased.
				if writeTooOldState.err != nil {
					writeTooOldState.err.ActualTimestamp.Forward(
						tErr.ActualTimestamp)
				} else {
					writeTooOldState.err = tErr
				}

				// For requests that are both read and write, we don't have the option
				// of leaving an intent behind when they encounter a WriteTooOldError,
				// so we have to return an error instead of a response with the
				// WriteTooOld flag set (which would also leave intents behind). These
				// requests need to be re-evaluated at the bumped timestamp in order for
				// their write to be valid. The current evaluation resulted in an intent
				// that could well be different from what the request would write if it
				// were evaluated at the bumped timestamp, which would cause the request
				// to be rejected if it were sent again with the same sequence number
				// after a refresh.
				// TODO(andrei): What we really want to do here is either speculatively
				// evaluate the request at the bumped timestamp and return that
				// speculative result, or leave behind a type of lock that wouldn't
				// prevent the request for evaluating again at the same sequence number
				// but at a bumped timestamp.
				if roachpb.IsReadAndWrite(args) {
					writeTooOldState.cantDeferWTOE = true
				}

				if baHeader.Txn != nil {
					baHeader.Txn.WriteTimestamp.Forward(tErr.ActualTimestamp)
					baHeader.Txn.WriteTooOld = true
				} else {
					// For non-transactional requests, there's nowhere to defer the error
					// to. And the request has to fail because non-transactional batches
					// should read and write at the same timestamp.
					writeTooOldState.cantDeferWTOE = true
				}

				// Clear pErr; we're done processing the WTOE for now and we'll return
				// to considering it below after we've evaluated all requests.
				pErr = nil
			default:
				return nil, mergedResult, pErr
			}
		}

		if maxKeys != math.MaxInt64 {
			retResults := reply.Header().NumKeys
			if retResults > maxKeys {
				log.Fatalf(ctx, "received %d results, limit was %d", retResults, maxKeys)
			}
			maxKeys -= retResults
		}

		// If transactional, we use ba.Txn for each individual command and
		// accumulate updates to it. Once accumulated, we then remove the Txn
		// from each individual response.
		// TODO(spencer,tschottdorf): need copy-on-write behavior for the
		//   updated batch transaction / timestamp.
		if baHeader.Txn != nil {
			if header := reply.Header(); header.Txn != nil {
				baHeader.Txn.Update(header.Txn)
				header.Txn = nil
				reply.SetHeader(header)
			}
		}
	}

	if writeTooOldState.err != nil {
		if baHeader.Txn != nil && baHeader.Txn.Status.IsCommittedOrStaging() {
			log.Fatalf(ctx, "committed txn with writeTooOld err: %s", writeTooOldState.err)
		}
	}

	// If there's a write too old error that we can't defer, return it.
	if writeTooOldState.cantDeferWTOE {
		return nil, mergedResult, roachpb.NewErrorWithTxn(writeTooOldState.err, baHeader.Txn)
	}

	// Update the batch response timestamp field to the timestamp at which the
	// batch's reads were evaluated.
	if baHeader.Txn != nil {
		// If transactional, send out the final transaction entry with the reply.
		br.Txn = baHeader.Txn
		// Note that br.Txn.ReadTimestamp might be higher than baHeader.Timestamp if
		// we had an EndTxn that decided that it can refresh to something higher
		// than baHeader.Timestamp because there were no refresh spans.
		if br.Txn.ReadTimestamp.Less(baHeader.Timestamp) {
			log.Fatalf(ctx, "br.Txn.ReadTimestamp < ba.Timestamp (%s < %s). ba: %s",
				br.Txn.ReadTimestamp, baHeader.Timestamp, ba)
		}
		br.Timestamp = br.Txn.ReadTimestamp
	} else {
		br.Timestamp = baHeader.Timestamp
	}

	return br, mergedResult, nil
}

// evaluateCommand delegates to the eval method for the given
// roachpb.Request. The returned Result may be partially valid
// even if an error is returned. maxKeys is the number of scan results
// remaining for this batch (MaxInt64 for no limit).
func evaluateCommand(
	ctx context.Context,
	raftCmdID storagebase.CmdIDKey,
	index int,
	readWriter engine.ReadWriter,
	rec batcheval.EvalContext,
	ms *enginepb.MVCCStats,
	h roachpb.Header,
	maxKeys int64,
	args roachpb.Request,
	reply roachpb.Response,
) (result.Result, *roachpb.Error) {
	// If a unittest filter was installed, check for an injected error; otherwise, continue.
	if filter := rec.EvalKnobs().TestingEvalFilter; filter != nil {
		filterArgs := storagebase.FilterArgs{
			Ctx:   ctx,
			CmdID: raftCmdID,
			Index: index,
			Sid:   rec.StoreID(),
			Req:   args,
			Hdr:   h,
		}
		if pErr := filter(filterArgs); pErr != nil {
			if pErr.GetTxn() == nil {
				pErr.SetTxn(h.Txn)
			}
			log.Infof(ctx, "test injecting error: %s", pErr)
			return result.Result{}, pErr
		}
	}

	var err error
	var pd result.Result

	if cmd, ok := batcheval.LookupCommand(args.Method()); ok {
		cArgs := batcheval.CommandArgs{
			EvalCtx: rec,
			Header:  h,
			Args:    args,
			MaxKeys: maxKeys,
			Stats:   ms,
		}

		if cmd.EvalRW != nil {
			pd, err = cmd.EvalRW(ctx, readWriter, cArgs, reply)
		} else {
			pd, err = cmd.EvalRO(ctx, readWriter, cArgs, reply)
		}
	} else {
		err = errors.Errorf("unrecognized command %s", args.Method())
	}

	if h.ReturnRangeInfo {
		returnRangeInfo(reply, rec)
	}

	// TODO(peter): We'd like to assert that the hlc clock is always updated
	// correctly, but various tests insert versioned data without going through
	// the proper channels. See TestPushTxnUpgradeExistingTxn for an example.
	//
	// if header.Txn != nil && h.Timestamp.LessEq(header.Txn.Timestamp) {
	// 	if now := r.store.Clock().Now(); now.Less(header.Txn.Timestamp) {
	// 		log.Fatalf(ctx, "hlc clock not updated: %s < %s", now, header.Txn.Timestamp)
	// 	}
	// }

	if log.V(2) {
		log.Infof(ctx, "evaluated %s command %+v: %+v, err=%v", args.Method(), args, reply, err)
	}

	// Create a roachpb.Error by initializing txn from the request/response header.
	var pErr *roachpb.Error
	if err != nil {
		txn := reply.Header().Txn
		if txn == nil {
			txn = h.Txn
		}
		pErr = roachpb.NewErrorWithTxn(err, txn)
	}

	return pd, pErr
}

// returnRangeInfo populates RangeInfos in the response if the batch
// requested them.
func returnRangeInfo(reply roachpb.Response, rec batcheval.EvalContext) {
	header := reply.Header()
	lease, _ := rec.GetLease()
	desc := rec.Desc()
	header.RangeInfos = []roachpb.RangeInfo{
		{
			Desc:  *desc,
			Lease: lease,
		},
	}
	reply.SetHeader(header)
}
