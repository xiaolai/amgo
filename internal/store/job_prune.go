package store

import (
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

// JobPruneReport describes terminal job retention. Paths and JobIDs are for the
// caller to remove owned files; they are not part of the JSON envelope.
type JobPruneReport struct {
	DryRun        bool     `json:"dry_run"`
	Jobs          int      `json:"jobs"`
	Events        int      `json:"events"`
	Artifacts     int      `json:"artifacts"`
	ArtifactBytes int64    `json:"artifact_bytes"`
	JobIDs        []string `json:"-"`
}

// PruneJobs removes terminal jobs older than olderThan, except unknown jobs,
// which are retained as unresolved evidence. Active jobs and jobs referenced by
// a lease are never removed.
func (s *Store) PruneJobs(now, olderThan time.Time, dryRun bool) (JobPruneReport, error) {
	rep := JobPruneReport{DryRun: dryRun}
	run := s.update
	if dryRun {
		run = s.view
	}
	err := run(func(tx *bolt.Tx) error {
		leased := map[string]bool{}
		if err := scanPrefixN(tx.Bucket(bucketLeases), nil, 0, func(_, v []byte) error {
			var l model.Lease
			if err := decodeJSON(v, &l); err != nil {
				return err
			}
			leased[l.JobID] = true
			return nil
		}); err != nil {
			return err
		}
		var victims []model.Job
		if err := scanPrefixN(tx.Bucket(bucketJobs), nil, 0, func(_, v []byte) error {
			var j model.Job
			if err := decodeJSON(v, &j); err != nil {
				return err
			}
			if !j.State.Terminal() || j.State == model.JobUnknown || j.FinishedAt == nil {
				return nil
			}
			if !j.FinishedAt.Before(olderThan) || leased[j.ID] {
				return nil
			}
			victims = append(victims, j)
			return nil
		}); err != nil {
			return err
		}
		for _, j := range victims {
			var eventKeys [][]byte
			if err := scanPrefixN(tx.Bucket(bucketJobEventByJob), compound(j.ID, ""), 0, func(k, v []byte) error {
				eventKeys = append(eventKeys, append([]byte(nil), k...))
				return nil
			}); err != nil {
				return err
			}
			var artifactKeys [][]byte
			if err := scanPrefixN(tx.Bucket(bucketArtifacts), compound(j.ID, ""), 0, func(k, v []byte) error {
				artifactKeys = append(artifactKeys, append([]byte(nil), k...))
				var a model.Artifact
				if err := decodeJSON(v, &a); err != nil {
					return err
				}
				rep.ArtifactBytes += a.Bytes
				return nil
			}); err != nil {
				return err
			}
			rep.Jobs++
			rep.Events += len(eventKeys)
			rep.Artifacts += len(artifactKeys)
			rep.JobIDs = append(rep.JobIDs, j.ID)
			if dryRun {
				continue
			}
			for _, k := range eventKeys {
				seq := tx.Bucket(bucketJobEventByJob).Get(k)
				if seq != nil {
					if err := tx.Bucket(bucketJobEvents).Delete(seq); err != nil {
						return err
					}
				}
				if err := tx.Bucket(bucketJobEventByJob).Delete(k); err != nil {
					return err
				}
			}
			for _, k := range artifactKeys {
				if err := tx.Bucket(bucketArtifacts).Delete(k); err != nil {
					return err
				}
			}
			if err := tx.Bucket(bucketJobs).Delete([]byte(j.ID)); err != nil {
				return err
			}
		}
		return nil
	})
	return rep, err
}
