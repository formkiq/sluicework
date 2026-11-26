package api

import (
	"context"
	"database/sql"
	"time"
)

type activityHour struct {
	Start    time.Time `json:"start"`
	Runs     int       `json:"runs"`
	Failures int       `json:"failures"`
}

type failedRun struct {
	ID        string    `json:"id"`
	JobName   string    `json:"jobName"`
	JobType   string    `json:"jobType"`
	MachineID string    `json:"machineId"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
}

type overviewData struct {
	Queued               int            `json:"queued"`
	Running              int            `json:"running"`
	Failed24h            int            `json:"failed24h"`
	UnhealthyConnections int            `json:"unhealthyConnections"`
	Hourly               []activityHour `json:"hourly"`
	RecentFailures       []failedRun    `json:"recentFailures"`
}

func (s *Server) overview(r *request) (any, error) {
	return loadOverview(r.Context(), s.State.DB, time.Now().UTC())
}

const failureStates = "('failed','timed_out','interrupted')"

func loadOverview(ctx context.Context, db *sql.DB, now time.Time) (overviewData, error) {
	start := now.UTC().Add(-24 * time.Hour).Truncate(time.Millisecond)
	result := overviewData{Hourly: make([]activityHour, 24), RecentFailures: []failedRun{}}
	for i := range result.Hourly {
		result.Hourly[i].Start = start.Add(time.Duration(i) * time.Hour)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status='queued'), count(*) FILTER (WHERE status='running'),
  (SELECT count(*) FROM provider_connections WHERE disabled=0 AND health='error') FROM runs WHERE status IN ('queued','running')`).Scan(&result.Queued, &result.Running, &result.UnhealthyConnections); err != nil {
		return result, err
	}
	// Include runs created in the same millisecond as the query in the newest bucket.
	rows, err := tx.QueryContext(ctx, `SELECT min(23,(created-?)/3600000), count(*), count(*) FILTER (WHERE status IN `+failureStates+`)
  FROM runs WHERE created BETWEEN ? AND ? GROUP BY 1`, start.UnixMilli(), start.UnixMilli(), now.UnixMilli())
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var index, total, failed int
		if err := rows.Scan(&index, &total, &failed); err != nil {
			rows.Close()
			return result, err
		}
		result.Hourly[index].Runs = total
		result.Hourly[index].Failures = failed
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	// The dashboard reports the terminal state of the latest attempt. A successful retry clears a failure.
	rows, err = tx.QueryContext(ctx, `SELECT r.id, json_extract(j.definition,'$.name'), json_extract(j.definition,'$.type'),
  coalesce(a.machine_id,''), r.status, r.created, count(*) FILTER (WHERE a.ended BETWEEN ? AND ?) OVER ()
  FROM runs r JOIN attempts a ON a.id=(SELECT id FROM attempts WHERE run_id=r.id ORDER BY number DESC LIMIT 1)
  JOIN job_revisions j ON j.job_id=r.job_id AND j.revision=r.revision
  WHERE r.status IN `+failureStates+` ORDER BY a.ended DESC,r.id DESC LIMIT 3`, start.UnixMilli(), now.UnixMilli())
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item failedRun
		var created int64
		if err := rows.Scan(&item.ID, &item.JobName, &item.JobType, &item.MachineID, &item.State, &created, &result.Failed24h); err != nil {
			rows.Close()
			return result, err
		}
		item.CreatedAt = time.UnixMilli(created).UTC()
		result.RecentFailures = append(result.RecentFailures, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	return result, tx.Commit()
}
