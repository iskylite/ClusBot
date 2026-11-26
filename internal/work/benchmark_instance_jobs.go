package work

import (
	"context"
	"database/sql"
	"errors"
)

// The routing identity is write-once. A confirmed job ID cannot silently be
// assigned to another actor, operation, or retry. Keep terminal bindings so
// replays (including reused provider IDs) cannot target newer work.
type instanceBenchmarkJob struct {
	source, job, principal, task, step, operation, idempotency, dispatchDigest string
}

func (j instanceBenchmarkJob) valid() bool {
	return j.source != "" && j.job != "" && j.principal != "" && j.task != "" && j.step != "" &&
		j.operation != "" && j.idempotency != "" && j.dispatchDigest != ""
}

func readInstanceBenchmarkJob(ctx context.Context, db benchmarkBindingReader, source, job string) (instanceBenchmarkJob, error) {
	data := instanceBenchmarkJob{source: source, job: job}
	err := db.QueryRowContext(ctx, `SELECT principal_id,task_id,step_id,operation_id,idempotency_key,dispatch_digest
FROM benchmark_instance_jobs WHERE source_key=? AND job_id=?`, source, job).
		Scan(&data.principal, &data.task, &data.step, &data.operation, &data.idempotency, &data.dispatchDigest)
	return data, err
}

// Account writer is already locked; acquire the instance writer second. Commit
// this identity before advancing the private case from submitting to following.
// If the private commit fails or the process dies between commits, lookup uses
// the same operation and idempotency key and may replay this exact binding.
func bindInstanceBenchmarkJob(ctx context.Context, instance *sql.DB, job instanceBenchmarkJob) (bool, error) {
	if !job.valid() {
		return false, ErrBenchmarkBinding
	}
	tx, err := instance.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO benchmark_instance_jobs
(source_key,job_id,principal_id,task_id,step_id,operation_id,idempotency_key,dispatch_digest)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, job.source, job.job, job.principal,
		job.task, job.step, job.operation, job.idempotency, job.dispatchDigest); err != nil {
		return false, err
	}
	bound, err := readInstanceBenchmarkJob(ctx, tx, job.source, job.job)
	if errors.Is(err, sql.ErrNoRows) {
		// The same operation or its idempotency key already has another job.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if bound != job {
		return false, nil
	}
	return true, tx.Commit()
}

func instanceBenchmarkJobMatches(ctx context.Context, instance *sql.DB, job instanceBenchmarkJob) (bool, error) {
	if !job.valid() {
		return false, nil
	}
	bound, err := readInstanceBenchmarkJob(ctx, instance, job.source, job.job)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return bound == job, err
}

func (d benchmarkDispatch) instanceJob(id string, op Operation) instanceBenchmarkJob {
	return instanceBenchmarkJob{d.jobSource, id, d.actor, d.task, d.step, d.operation, d.request.IdempotencyKey, op.ParamsDigest}
}
