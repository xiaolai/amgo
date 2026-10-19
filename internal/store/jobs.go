package store

import (
	"encoding/binary"
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

// CreateJob persists a new job and its creation event in one transaction.
func (s *Store) CreateJob(job model.Job, now time.Time) (model.Job, error) {
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		if b.Get([]byte(job.ID)) != nil {
			return model.NewError(model.ErrInternal, "job id already exists")
		}
		job.RecordVersion = 1
		job.UpdatedAt = now
		if err := putJSON(b, []byte(job.ID), job); err != nil {
			return err
		}
		return appendJobEvent(tx, job.ID, "created", now, map[string]any{
			"adapter": job.Adapter, "state": string(job.State),
		})
	})
	return job, err
}

func (s *Store) GetJob(id string) (model.Job, error) {
	var out model.Job
	err := s.view(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketJobs), []byte(id), &out)
	})
	return out, err
}

// UpdateJob applies fn to the stored record in one write transaction and
// appends a state event when the state changed. The caller must hold the job's
// lease or be its supervisor; callers re-read state inside fn to make the
// transition a check-and-set.
func (s *Store) UpdateJob(id string, now time.Time, fn func(*model.Job) error) (model.Job, error) {
	var out model.Job
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		if err := getJSON(b, []byte(id), &out); err != nil {
			return err
		}
		before := out.State
		if err := fn(&out); err != nil {
			return err
		}
		out.UpdatedAt = now
		if err := putJSON(b, []byte(id), out); err != nil {
			return err
		}
		if out.State != before {
			return appendJobEvent(tx, id, "state", now, map[string]any{
				"from": string(before), "to": string(out.State),
			})
		}
		return nil
	})
	return out, err
}

// ListJobs returns jobs newest first. UUIDv7 identifiers sort by creation time.
func (s *Store) ListJobs(limit int) ([]model.Job, error) {
	var out []model.Job
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketJobs).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var j model.Job
			if err := decodeJSON(v, &j); err != nil {
				return err
			}
			out = append(out, j)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

// ListActiveJobs returns non-terminal jobs oldest first.
func (s *Store) ListActiveJobs() ([]model.Job, error) {
	var out []model.Job
	err := s.view(func(tx *bolt.Tx) error {
		return scanPrefixN(tx.Bucket(bucketJobs), nil, 0, func(_, v []byte) error {
			var j model.Job
			if err := decodeJSON(v, &j); err != nil {
				return err
			}
			if !j.State.Terminal() {
				out = append(out, j)
			}
			return nil
		})
	})
	return out, err
}

// AppendJobEvent records a bounded progress event. Raw provider output belongs
// in the job log, not here.
func (s *Store) AppendJobEvent(jobID, kind string, now time.Time, payload map[string]any) (model.JobEvent, error) {
	ev := model.JobEvent{JobID: jobID, Kind: kind, At: now, Payload: payload}
	err := s.update(func(tx *bolt.Tx) error {
		return appendJobEventInto(tx, &ev)
	})
	return ev, err
}

func appendJobEvent(tx *bolt.Tx, jobID, kind string, now time.Time, payload map[string]any) error {
	ev := model.JobEvent{JobID: jobID, Kind: kind, At: now, Payload: payload}
	return appendJobEventInto(tx, &ev)
}

func appendJobEventInto(tx *bolt.Tx, ev *model.JobEvent) error {
	meta := tx.Bucket(bucketMeta)
	var seq uint64
	if raw := meta.Get([]byte("job_event_seq")); raw != nil && len(raw) == 8 {
		seq = binary.BigEndian.Uint64(raw)
	}
	seq++
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	if err := meta.Put([]byte("job_event_seq"), b); err != nil {
		return err
	}
	ev.Seq = seq
	if err := putJSON(tx.Bucket(bucketJobEvents), b, ev); err != nil {
		return err
	}
	return tx.Bucket(bucketJobEventByJob).Put(compound(ev.JobID, string(b)), b)
}

// ListJobEvents returns events with Seq > after, oldest first.
func (s *Store) ListJobEvents(jobID string, after uint64, limit int) ([]model.JobEvent, error) {
	var out []model.JobEvent
	err := s.view(func(tx *bolt.Tx) error {
		idx := tx.Bucket(bucketJobEventByJob)
		main := tx.Bucket(bucketJobEvents)
		prefix := compound(jobID, "")
		return scanPrefixN(idx, prefix, 0, func(k, v []byte) error {
			seq := binary.BigEndian.Uint64(v)
			if seq <= after {
				return nil
			}
			var ev model.JobEvent
			if err := getJSON(main, v, &ev); err != nil {
				return err
			}
			out = append(out, ev)
			if limit > 0 && len(out) >= limit {
				return errStopScan
			}
			return nil
		})
	})
	return out, err
}

// AcquireLease takes the single amgo write lease for a checkout. A lease whose
// owner is still alive and unexpired returns workspace_busy; a stale lease is
// replaced. alive is nil when liveness cannot be checked, in which case only
// the expiry is trusted.
func (s *Store) AcquireLease(l model.Lease, now time.Time, alive func(pid int, procStart string) bool) (model.Lease, error) {
	var out model.Lease
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketLeases)
		var existing model.Lease
		found, err := getJSONOK(b, []byte(l.CheckoutID), &existing)
		if err != nil {
			return err
		}
		if found && existing.JobID != l.JobID {
			live := existing.Held || now.Before(existing.ExpiresAt)
			if live && !existing.Held && alive != nil && existing.OwnerPID > 0 {
				live = alive(existing.OwnerPID, existing.OwnerProcStart)
			}
			if live {
				return &model.CodedError{
					Code: model.ErrWorkspaceBusy, Message: "workspace has an active amgo job",
					Details: map[string]any{"checkout_id": l.CheckoutID, "job_id": existing.JobID},
				}
			}
		}
		l.RecordVersion = 1
		if found && existing.JobID == l.JobID {
			l.AcquiredAt = existing.AcquiredAt
		}
		if l.AcquiredAt.IsZero() {
			l.AcquiredAt = now
		}
		if err := putJSON(b, []byte(l.CheckoutID), l); err != nil {
			return err
		}
		out = l
		return appendJobEvent(tx, l.JobID, "lease_acquired", now, map[string]any{
			"checkout_id": l.CheckoutID, "expires_at": l.ExpiresAt,
		})
	})
	return out, err
}

func (s *Store) GetLease(checkoutID string) (model.Lease, bool, error) {
	var out model.Lease
	found := false
	err := s.view(func(tx *bolt.Tx) error {
		var err error
		found, err = getJSONOK(tx.Bucket(bucketLeases), []byte(checkoutID), &out)
		return err
	})
	return out, found, err
}

// ReleaseLease removes only this job's lease; a stale job cannot release a
// newer owner's lease.
func (s *Store) ReleaseLease(checkoutID, jobID string, now time.Time) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketLeases)
		var existing model.Lease
		found, err := getJSONOK(b, []byte(checkoutID), &existing)
		if err != nil || !found || existing.JobID != jobID {
			return err
		}
		if err := b.Delete([]byte(checkoutID)); err != nil {
			return err
		}
		return appendJobEvent(tx, jobID, "lease_released", now, map[string]any{"checkout_id": checkoutID})
	})
}

// HoldLease marks this job's lease as unresolved. A held lease never expires
// into another writer's hands; ReleaseLease or an operator risk acceptance is
// required to remove it.
func (s *Store) HoldLease(checkoutID, jobID, reason string, now time.Time) error {
	return s.update(func(tx *bolt.Tx) error {
		var job model.Job
		if err := getJSON(tx.Bucket(bucketJobs), []byte(jobID), &job); err != nil {
			return err
		}
		if job.State.Terminal() {
			// A terminal job's lease is not unresolved work; never revive a hold
			// from a stale finalization path.
			return nil
		}
		b := tx.Bucket(bucketLeases)
		var existing model.Lease
		found, err := getJSONOK(b, []byte(checkoutID), &existing)
		if err != nil || !found || existing.JobID != jobID {
			return err
		}
		existing.Held = true
		existing.HoldReason = reason
		if err := putJSON(b, []byte(checkoutID), existing); err != nil {
			return err
		}
		return appendJobEvent(tx, jobID, "lease_held", now, map[string]any{
			"checkout_id": checkoutID, "reason": reason,
		})
	})
}

// JobHoldExpect is the snapshot a recovery decision was based on. A hold is
// applied only when the live record still matches it.
type JobHoldExpect struct {
	State                  model.JobState
	OwnerPID               int
	OwnerProcStart         string
	ChildPID               int
	ChildProcStart         string
	ProviderStartRecorded  bool
	ProviderStartAttempted bool
	StartedAt              *time.Time
}

// JobMatchesExpect reports whether a live record still matches a recovery
// snapshot.
func JobMatchesExpect(j model.Job, e JobHoldExpect) bool { return expectMatches(j, e) }

func expectMatches(j model.Job, e JobHoldExpect) bool {
	return j.State == e.State &&
		j.OwnerPID == e.OwnerPID &&
		j.OwnerProcStart == e.OwnerProcStart &&
		j.ChildPID == e.ChildPID &&
		j.ChildProcStart == e.ChildProcStart &&
		j.ProviderStartRecorded == e.ProviderStartRecorded &&
		j.ProviderStartAttempted == e.ProviderStartAttempted &&
		sameTime(j.StartedAt, e.StartedAt)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// HoldJobLease marks a non-terminal job and its lease unresolved in one
// transaction. It refuses to touch a terminal job. When expect is non-nil, the
// live record must still match the snapshot the caller acted on; a concurrent
// supervisor start therefore cannot be finalized from a stale read. The bool
// reports whether the hold was applied.
func (s *Store) HoldJobLease(jobID, checkoutID, reason string, code string, expect *JobHoldExpect, now time.Time) (model.Job, bool, error) {
	var out model.Job
	held := false
	err := s.update(func(tx *bolt.Tx) error {
		jobs := tx.Bucket(bucketJobs)
		if err := getJSON(jobs, []byte(jobID), &out); err != nil {
			return err
		}
		if out.State.Terminal() {
			return nil
		}
		if expect != nil && !expectMatches(out, *expect) {
			return nil
		}
		leases := tx.Bucket(bucketLeases)
		var l model.Lease
		found, err := getJSONOK(leases, []byte(checkoutID), &l)
		if err != nil {
			return err
		}
		if !found || l.JobID != jobID {
			return model.NewError(model.ErrInternal, "cannot hold a lease that this job does not own")
		}
		if l.Held && out.LeaseHeld && l.HoldReason == reason && out.LeaseHoldReason == reason {
			// Recovery re-examines held jobs on every command; do not append a
			// new event for an unchanged hold.
			held = true
			return nil
		}
		l.Held = true
		l.HoldReason = reason
		if err := putJSON(leases, []byte(checkoutID), l); err != nil {
			return err
		}
		out.LeaseHeld = true
		out.LeaseHoldReason = reason
		out.ResultUnverified = true
		out.ErrorCode = code
		out.ErrorDetail = reason
		out.UpdatedAt = now
		if err := putJSON(jobs, []byte(jobID), out); err != nil {
			return err
		}
		held = true
		return appendJobEvent(tx, jobID, "lease_held", now, map[string]any{
			"checkout_id": checkoutID, "reason": reason,
		})
	})
	return out, held, err
}

// ReleaseJobLease removes this job's lease and clears its hold markers in one
// transaction. It is used for normal completion and for an operator who
// explicitly accepts unconfirmed termination.
func (s *Store) ReleaseJobLease(jobID, checkoutID string, now time.Time) (model.Job, error) {
	var out model.Job
	err := s.update(func(tx *bolt.Tx) error {
		jobs := tx.Bucket(bucketJobs)
		if err := getJSON(jobs, []byte(jobID), &out); err != nil {
			return err
		}
		leases := tx.Bucket(bucketLeases)
		var l model.Lease
		found, err := getJSONOK(leases, []byte(checkoutID), &l)
		if err != nil {
			return err
		}
		released := false
		if found && l.JobID == jobID {
			if err := leases.Delete([]byte(checkoutID)); err != nil {
				return err
			}
			released = true
		}
		out.LeaseHeld = false
		out.LeaseHoldReason = ""
		out.UpdatedAt = now
		if err := putJSON(jobs, []byte(jobID), out); err != nil {
			return err
		}
		if released {
			return appendJobEvent(tx, jobID, "lease_released", now, map[string]any{"checkout_id": checkoutID})
		}
		return nil
	})
	return out, err
}

func (s *Store) RecordArtifact(a model.Artifact, now time.Time) (model.Artifact, error) {
	a.RecordVersion = 1
	a.CreatedAt = now
	err := s.update(func(tx *bolt.Tx) error {
		key := compound(a.JobID, a.ID)
		if err := putJSON(tx.Bucket(bucketArtifacts), key, a); err != nil {
			return err
		}
		return appendJobEvent(tx, a.JobID, "artifact", now, map[string]any{
			"artifact_id": a.ID, "kind": a.Kind, "path": a.Path, "bytes": a.Bytes, "truncated": a.Truncated,
		})
	})
	return a, err
}

func (s *Store) ListArtifacts(jobID string) ([]model.Artifact, error) {
	var out []model.Artifact
	err := s.view(func(tx *bolt.Tx) error {
		prefix := compound(jobID, "")
		return scanPrefixN(tx.Bucket(bucketArtifacts), prefix, 0, func(_, v []byte) error {
			var a model.Artifact
			if err := decodeJSON(v, &a); err != nil {
				return err
			}
			out = append(out, a)
			return nil
		})
	})
	return out, err
}
