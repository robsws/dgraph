// +build !oss

/*
 * Copyright 2020 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Dgraph Community License (the "License"); you
 * may not use this file except in compliance with the License. You
 * may obtain a copy of the License at
 *
 *     https://github.com/dgraph-io/dgraph/blob/master/licenses/DCL.txt
 */

package worker

import (
	"context"

	"github.com/dgraph-io/dgraph/conn"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/protos/pb"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
)

// ProcessRestoreRequest verifies the backup data and sends a restore proposal to each group.
func ProcessRestoreRequest(ctx context.Context, req *pb.RestoreRequest) error {
	if req == nil {
		return errors.Errorf("restore request cannot be nil")
	}

	UpdateMembershipState(ctx)
	memState := GetMembershipState()

	currentGroups := make([]uint32, 0)
	for gid := range memState.GetGroups() {
		currentGroups = append(currentGroups, gid)
	}

	creds := Credentials{
		AccessKey:    req.AccessKey,
		SecretKey:    req.SecretKey,
		SessionToken: req.SessionToken,
		Anonymous:    req.Anonymous,
	}
	if err := VerifyBackup(req.Location, req.BackupId, &creds, currentGroups); err != nil {
		return errors.Wrapf(err, "failed to verify backup")
	}

	if err := FillRestoreCredentials(req.Location, req); err != nil {
		return errors.Wrapf(err, "cannot fill restore proposal with the right credentials")
	}
	req.RestoreTs = State.GetTimestamp(false)

	cancelCtx, cancel := context.WithCancel(ctx)
	for _, gid := range currentGroups {
		reqCopy := proto.Clone(req).(*pb.RestoreRequest)
		reqCopy.GroupId = gid
		if err := proposeRestoreOrSend(cancelCtx, req); err != nil {
			cancel()
			return err
		}
	}

	return nil
}

func proposeRestoreOrSend(ctx context.Context, req *pb.RestoreRequest) error {
	if groups().ServesGroup(req.GetGroupId()) {
		_, err := (&grpcWorker{}).Restore(ctx, req)
		return err
	}

	pl := groups().Leader(req.GetGroupId())
	if pl == nil {
		return conn.ErrNoConnection
	}
	con := pl.Get()
	c := pb.NewWorkerClient(con)

	_, err := c.Restore(ctx, req)
	return err
}

// Restore implements the Worker interface.
func (w *grpcWorker) Restore(ctx context.Context, req *pb.RestoreRequest) (*pb.Status, error) {
	var emptyRes pb.Status
	// TODO: add tracing to backup and restore operations.

	if !groups().ServesGroup(req.GroupId) {
		return &emptyRes, errors.Errorf("this server doesn't serve group id: %v", req.GroupId)
	}

	// We should wait to ensure that we have seen all the updates until the StartTs
	// of this restore transaction.
	if err := posting.Oracle().WaitForTs(ctx, req.RestoreTs); err != nil {
		return nil, errors.Wrapf(err, "cannot wait for restore ts %d", req.RestoreTs)
	}

	err := groups().Node.proposeAndWait(ctx, &pb.Proposal{Restore: req})
	if err != nil {
		return &emptyRes, errors.Wrapf(err, "cannot propose restore request")
	}

	return &emptyRes, nil
}

func handleRestoreProposal(ctx context.Context, req *pb.RestoreRequest) error {
	if req == nil {
		return errors.Errorf("nil restore request")
	}

	// Drop all the current data. This also cancels all existing transactions.
	dropProposal := pb.Proposal{
		Mutations: &pb.Mutations{
			GroupId: req.GroupId,
			StartTs: req.RestoreTs,
			DropOp:  pb.Mutations_ALL,
		},
	}
	if err := groups().Node.applyMutations(ctx, &dropProposal); err != nil {
		return err
	}

	// Reset tablets and set correct tablets to match the restored backup.
	creds := &Credentials{
		AccessKey:    req.AccessKey,
		SecretKey:    req.SecretKey,
		SessionToken: req.SessionToken,
		Anonymous:    req.Anonymous,
	}
	handler, err := NewUriHandler(req.Location, creds)
	manifest, err := handler.GetLatestManifest(req.BackupId)

	// stream database

	// update timestamp.

	// Propose a snapshot immediately after all the work is done to prevent the restore
	// from being replayed.
	// TODO: is this enough to successfully trigger the snapshot?
	if err := groups().Node.proposeSnapshot(0); err != nil {
		return err
	}

	return nil
}
