package workspacefs

import (
	"context"
	"errors"
)

func (s *DeliveryFS) verifyDelivery(ctx context.Context, snap DeliverySnapshot) (deliveryGit, error) {
	if !validCommit(snap.BaseCommit) {
		return deliveryGit{}, errors.New("invalid delivery base")
	}
	mirror, err := s.mirrorGit(snap.MirrorPath)
	if err != nil {
		return deliveryGit{}, err
	}
	if snap.Commit == "" {
		return mirror, nil
	}
	if !validCommit(snap.Commit) || len(snap.Commit) != len(snap.BaseCommit) {
		return deliveryGit{}, errors.New("invalid delivery commit")
	}
	ref, err := deliveryRef(snap.ProjectID, snap.DeliveryID)
	if err != nil {
		return deliveryGit{}, err
	}
	oid, err := mirror.value(ctx, "show-ref", "--verify", "--hash", ref)
	if err != nil {
		return deliveryGit{}, err
	}
	if oid != snap.Commit {
		return deliveryGit{}, errors.New("delivery ref does not match its record")
	}
	parents, err := mirror.value(ctx, "rev-list", "--parents", "-n", "1", snap.Commit)
	if err != nil {
		return deliveryGit{}, err
	}
	if parents != snap.Commit+" "+snap.BaseCommit {
		return deliveryGit{}, errors.New("delivery commit does not have the recorded base as its sole parent")
	}
	return mirror, nil
}

func deliveryDiffArgs() []string {
	return []string{"diff", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color",
		"--src-prefix=a/", "--dst-prefix=b/", "--submodule=short", "--ignore-submodules=none"}
}

// DeliveryChanges lists bounded, base-relative changes from the mirror.
func (s *DeliveryFS) DeliveryChanges(ctx context.Context, snap DeliverySnapshot) (Changes, error) {
	out := Changes{BaseCommit: snap.BaseCommit, Files: []Change{}}
	if snap.Commit == "" {
		return out, nil
	}
	git, err := s.verifyDelivery(ctx, snap)
	if err != nil {
		return Changes{}, err
	}
	status, err := git.run(ctx, nil, MaxDiffBytes, append(deliveryDiffArgs(), "--name-status", "-z", snap.BaseCommit, snap.Commit, "--")...)
	if err != nil {
		return Changes{}, err
	}
	stats, err := git.run(ctx, nil, MaxDiffBytes, append(deliveryDiffArgs(), "--numstat", "-z", snap.BaseCommit, snap.Commit, "--")...)
	if err != nil {
		return Changes{}, err
	}
	populateChanges(&out, status, stats)
	return out, nil
}

// DeliveryDiff returns a bounded unified diff for one literal path from the mirror.
func (s *DeliveryFS) DeliveryDiff(ctx context.Context, snap DeliverySnapshot, path string) (Diff, error) {
	rel, err := Clean(path)
	if err != nil {
		return Diff{}, err
	}
	if rel == "" {
		return Diff{}, ErrNotFile
	}
	if snap.Commit == "" {
		return Diff{Path: rel}, nil
	}
	git, err := s.verifyDelivery(ctx, snap)
	if err != nil {
		return Diff{}, err
	}
	patch, err := git.run(ctx, nil, MaxDiffBytes, append(deliveryDiffArgs(), snap.BaseCommit, snap.Commit, "--", ":(literal)"+rel)...)
	if err != nil {
		return Diff{}, err
	}
	return Diff{Path: rel, Patch: string(patch)}, nil
}

// DeliveryPatch returns a binary-capable Git patch. ErrTooLarge is returned without any partial
// output when the patch exceeds maxBytes.
func (s *DeliveryFS) DeliveryPatch(ctx context.Context, snap DeliverySnapshot, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("invalid patch size limit")
	}
	if snap.Commit == "" {
		return nil, nil
	}
	git, err := s.verifyDelivery(ctx, snap)
	if err != nil {
		return nil, err
	}
	args := append(deliveryDiffArgs(), "--binary", "--full-index", snap.BaseCommit, snap.Commit, "--")
	patch, err := git.run(ctx, nil, maxBytes, args...)
	if err != nil {
		return nil, err
	}
	return patch, nil
}
