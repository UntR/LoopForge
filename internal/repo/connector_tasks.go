package repo

import (
	"encoding/json"
	"errors"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrConnectorTaskNotFound = errors.New("connector task not found")
	ErrConnectorTaskConflict = errors.New("connector task idempotency conflict")
	ErrConnectorTaskLease    = errors.New("connector task lease lost or mismatched")
	ErrConnectorAgentBusy    = errors.New("connector Agent already holds an active task")
)

const connectorClaimTTL = 30 * time.Second
const connectorRunLeaseTTL = 45 * time.Second

type connectorTaskRow struct {
	ID              string `gorm:"primaryKey;size:64"`
	OwnerID         int64  `gorm:"uniqueIndex:idx_connector_task_request,priority:1;index"`
	IdempotencyHash string `gorm:"uniqueIndex:idx_connector_task_request,priority:2;size:64"`
	RequestDigest   string `gorm:"size:64"`
	AgentID         string `gorm:"index:idx_connector_task_queue,priority:1;size:64"`
	Provider        string `gorm:"size:32"`
	Status          string `gorm:"index:idx_connector_task_queue,priority:2;size:32"`
	Attempt         int
	LeaseHash       *string `gorm:"uniqueIndex;size:64"`
	LeaseUntil      *time.Time
	DeadlineAt      *time.Time
	CancelRequested bool
	RecoveryReason  string    `gorm:"size:32"`
	Input           []byte    `gorm:"type:json"`
	Output          []byte    `gorm:"type:json"`
	OutputDigest    string    `gorm:"size:64"`
	CreatedAt       time.Time `gorm:"index:idx_connector_task_queue,priority:3"`
	UpdatedAt       time.Time
}

func (connectorTaskRow) TableName() string { return "connector_tasks" }

// MySQL is the single authority for expiry, even if API replica clocks differ.
// Tests use the explicitly supplied clock. A failed clock read never authorizes work.
func connectorTaskNow(tx *gorm.DB, fallback time.Time) (time.Time, error) {
	if tx.Dialector.Name() != "mysql" {
		return fallback.UTC(), nil
	}
	var now time.Time
	if err := tx.Raw("SELECT UTC_TIMESTAMP(3)").Scan(&now).Error; err != nil {
		return now, err
	}
	if now.IsZero() {
		return now, errors.New("connector task database clock unavailable")
	}
	return now.UTC(), nil
}

func connectorTaskFromRow(row connectorTaskRow, detail bool, now time.Time) (domain.ConnectorTask, error) {
	task := domain.ConnectorTask{ID: row.ID, AgentID: row.AgentID, Provider: row.Provider, Status: row.Status, Attempt: row.Attempt, CancelRequested: row.CancelRequested, RecoveryReason: row.RecoveryReason, LeaseUntil: row.LeaseUntil, DeadlineAt: row.DeadlineAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.LeaseUntil != nil && row.LeaseUntil.After(now) {
		task.LeaseRemainingMillis = row.LeaseUntil.Sub(now).Milliseconds()
	}
	task.RunAllowed = row.Status == domain.ConnectorTaskRunning && !row.CancelRequested && task.LeaseRemainingMillis > 0 && row.DeadlineAt != nil && row.DeadlineAt.After(now)
	if detail {
		var input domain.ConnectorTaskRequest
		if err := json.Unmarshal(row.Input, &input); err != nil {
			return task, err
		}
		task.Input = &input
		if len(row.Output) > 0 {
			var result domain.ConnectorTaskResult
			if err := json.Unmarshal(row.Output, &result); err != nil {
				return task, err
			}
			task.Result = &result
		}
	}
	return task, nil
}

func (r *ConnectorTasksRepo) SubmitConnectorTask(ownerID int64, id string, input domain.ConnectorTaskRequest, clock time.Time) (domain.ConnectorTask, bool, error) {
	var task domain.ConnectorTask
	created := false
	input, err := domain.NormalizeConnectorTaskRequest(input)
	if err != nil {
		return task, false, err
	}
	if ownerID <= 0 || id == "" {
		return task, false, ErrConnectorTaskNotFound
	}
	body, err := json.Marshal(input)
	if err != nil {
		return task, false, err
	}
	digest := domain.ConnectorTokenHash(string(body))
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		now, err := connectorTaskNow(tx, clock)
		if err != nil {
			return err
		}
		var existing connectorTaskRow
		err = tx.Where("owner_id = ? AND idempotency_hash = ?", ownerID, domain.ConnectorTokenHash(input.IdempotencyKey)).Take(&existing).Error
		if err == nil {
			if existing.RequestDigest != digest {
				return ErrConnectorTaskConflict
			}
			task, err = connectorTaskFromRow(existing, true, now)
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		agent, credentialUntil, err := r.connectors.connectorTaskPrincipal(tx, "", input.AgentID, ownerID, now)
		if err != nil {
			if errors.Is(err, ErrConnectorCredential) {
				return ErrConnectorTaskNotFound
			}
			return err
		}
		now, err = connectorTaskNow(tx, now)
		if err != nil {
			return err
		}
		if !credentialUntil.After(now) {
			return ErrConnectorTaskNotFound
		}
		row := connectorTaskRow{ID: id, OwnerID: ownerID, IdempotencyHash: domain.ConnectorTokenHash(input.IdempotencyKey), RequestDigest: digest, AgentID: agent.ID, Provider: agent.Provider, Status: domain.ConnectorTaskQueued, Input: body, CreatedAt: now, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		var stored connectorTaskRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_id = ? AND idempotency_hash = ?", ownerID, domain.ConnectorTokenHash(input.IdempotencyKey)).Take(&stored).Error; err != nil {
			return err
		}
		if stored.RequestDigest != digest {
			return ErrConnectorTaskConflict
		}
		created = stored.ID == id
		if created {
			if err := appendConnectorTaskEvent(tx, stored, "", stored.Status, "submitted", false, now); err != nil {
				return err
			}
		}
		task, err = connectorTaskFromRow(stored, true, now)
		return err
	})
	return task, created, err
}

// A never-started claim may be retried. Once start has committed, loss of
// contact means unknown execution, not permission for a second worker to run.
func expireConnectorTasks(query *gorm.DB, now time.Time) error {
	var rows []connectorTaskRow
	if err := query.Session(&gorm.Session{}).Select("id", "owner_id", "agent_id", "attempt", "status", "lease_until", "deadline_at").Where("status IN ? AND (lease_until <= ? OR deadline_at <= ?)", []string{domain.ConnectorTaskClaimed, domain.ConnectorTaskRunning, domain.ConnectorTaskCancelRequested}, now, now).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		status, reason := domain.ConnectorTaskRecoveryRequired, "execution_unconfirmed"
		updates := map[string]any{"status": status, "updated_at": now, "recovery_reason": reason}
		if row.Status == domain.ConnectorTaskClaimed {
			status = domain.ConnectorTaskQueued
			updates["status"] = status
			updates["recovery_reason"] = ""
			updates["lease_hash"] = nil
			updates["lease_until"] = nil
		}
		result := query.Session(&gorm.Session{}).Model(&connectorTaskRow{}).Where("id = ? AND status = ? AND (lease_until <= ? OR deadline_at <= ?)", row.ID, row.Status, now, now).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			if err := appendConnectorTaskEvent(query.Session(&gorm.Session{NewDB: true}), row, row.Status, status, "lease_expired", false, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ConnectorTasksRepo) ConnectorTask(ownerID int64, id string, clock time.Time) (domain.ConnectorTask, error) {
	var task domain.ConnectorTask
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		now, err := connectorTaskNow(tx, clock)
		if err != nil {
			return err
		}
		if err := expireConnectorTasks(tx.Where("owner_id = ? AND id = ?", ownerID, id), now); err != nil {
			return err
		}
		var row connectorTaskRow
		if err := tx.Where("owner_id = ? AND id = ?", ownerID, id).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConnectorTaskNotFound
			}
			return err
		}
		task, err = connectorTaskFromRow(row, true, now)
		return err
	})
	return task, err
}

func (r *ConnectorTasksRepo) ConnectorTaskList(ownerID int64, clock time.Time) ([]domain.ConnectorTask, error) {
	out := []domain.ConnectorTask{}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		now, err := connectorTaskNow(tx, clock)
		if err != nil {
			return err
		}
		if err := expireConnectorTasks(tx.Where("owner_id = ?", ownerID), now); err != nil {
			return err
		}
		var rows []connectorTaskRow
		if err := tx.Where("owner_id = ?", ownerID).Order("created_at DESC, id DESC").Limit(100).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			task, err := connectorTaskFromRow(row, false, now)
			if err != nil {
				return err
			}
			out = append(out, task)
		}
		return nil
	})
	return out, err
}

func (r *ConnectorTasksRepo) ClaimConnectorTask(agentHash, leaseToken string, clock time.Time) (*domain.ConnectorTask, error) {
	if !domain.ValidConnectorToken(leaseToken, domain.ConnectorLeaseTokenPrefix) {
		return nil, ErrConnectorTaskLease
	}
	leaseHash := domain.ConnectorTokenHash(leaseToken)
	var task *domain.ConnectorTask
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		now, err := connectorTaskNow(tx, clock)
		if err != nil {
			return err
		}
		agent, credentialUntil, err := r.connectors.connectorTaskPrincipal(tx, agentHash, "", 0, now)
		if err != nil {
			return err
		}
		now, err = connectorTaskNow(tx, now)
		if err != nil {
			return err
		}
		if !credentialUntil.After(now) {
			return ErrConnectorCredential
		}
		if err := expireConnectorTasks(tx.Where("agent_id = ?", agent.ID), now); err != nil {
			return err
		}
		var prior connectorTaskRow
		err = tx.Where("lease_hash = ?", leaseHash).Take(&prior).Error
		if err == nil {
			if prior.AgentID != agent.ID {
				return ErrConnectorTaskLease
			}
			value, err := connectorTaskFromRow(prior, true, now)
			task = &value
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var active int64
		if err := tx.Model(&connectorTaskRow{}).Where("agent_id = ? AND status IN ?", agent.ID, []string{domain.ConnectorTaskClaimed, domain.ConnectorTaskRunning, domain.ConnectorTaskCancelRequested, domain.ConnectorTaskRecoveryRequired}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return ErrConnectorAgentBusy
		}
		var row connectorTaskRow
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("agent_id = ? AND status = ?", agent.ID, domain.ConnectorTaskQueued).Order("created_at ASC, id ASC").Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		now, err = connectorTaskNow(tx, now)
		if err != nil {
			return err
		}
		if !credentialUntil.After(now) {
			return ErrConnectorCredential
		}
		until := now.Add(connectorClaimTTL)
		result := tx.Model(&connectorTaskRow{}).Where("id = ? AND status = ?", row.ID, domain.ConnectorTaskQueued).Updates(map[string]any{"status": domain.ConnectorTaskClaimed, "attempt": row.Attempt + 1, "lease_hash": leaseHash, "lease_until": until, "deadline_at": nil, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConnectorTaskLease
		}
		from := row.Status
		row.Status = domain.ConnectorTaskClaimed
		row.Attempt++
		row.LeaseHash = &leaseHash
		row.LeaseUntil = &until
		row.UpdatedAt = now
		if err := appendConnectorTaskEvent(tx, row, from, row.Status, "claimed", true, now); err != nil {
			return err
		}
		value, err := connectorTaskFromRow(row, true, now)
		task = &value
		return err
	})
	return task, err
}

func (r *ConnectorTasksRepo) CancelConnectorTask(ownerID int64, id string, clock time.Time) (domain.ConnectorTask, error) {
	var task domain.ConnectorTask
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		now, err := connectorTaskNow(tx, clock)
		if err != nil {
			return err
		}
		if err := expireConnectorTasks(tx.Where("owner_id = ? AND id = ?", ownerID, id), now); err != nil {
			return err
		}
		var row connectorTaskRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_id = ? AND id = ?", ownerID, id).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConnectorTaskNotFound
			}
			return err
		}
		if !domain.ConnectorTaskTerminal(row.Status) {
			status := domain.ConnectorTaskCancelRequested
			if row.Status == domain.ConnectorTaskQueued {
				status = domain.ConnectorTaskCanceled
			}
			if row.Status == domain.ConnectorTaskRecoveryRequired {
				status = domain.ConnectorTaskRecoveryRequired
			}
			changed := row.Status != status || !row.CancelRequested
			if changed {
				if err := appendConnectorTaskEvent(tx, row, row.Status, status, "cancel_requested", false, now); err != nil {
					return err
				}
			}
			if err := tx.Model(&row).Updates(map[string]any{"status": status, "cancel_requested": true, "updated_at": now}).Error; err != nil {
				return err
			}
			row.Status = status
			row.CancelRequested = true
			row.UpdatedAt = now
		}
		task, err = connectorTaskFromRow(row, true, now)
		return err
	})
	return task, err
}

// Every mutation authenticates the Agent again inside its transaction and
// fences both the random lease proof and monotonically increasing attempt.
func (r *ConnectorTasksRepo) ConnectorTaskAction(agentHash, id, action string, proof domain.ConnectorTaskLease, completion *domain.ConnectorTaskResult, clock time.Time) (domain.ConnectorTask, error) {
	var task domain.ConnectorTask
	if proof.Attempt < 1 || !domain.ValidConnectorToken(proof.LeaseToken, domain.ConnectorLeaseTokenPrefix) {
		return task, ErrConnectorTaskLease
	}
	var body []byte
	var outputDigest string
	if action == "complete" {
		if completion == nil {
			return task, ErrConnectorTaskLease
		}
		normalized, err := domain.NormalizeConnectorTaskResult(*completion)
		if err != nil {
			return task, err
		}
		completion = &normalized
		body, _ = json.Marshal(normalized)
		outputDigest = domain.ConnectorTokenHash(string(body))
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		now, err := connectorTaskNow(tx, clock)
		if err != nil {
			return err
		}
		agent, credentialUntil, err := r.connectors.connectorTaskPrincipal(tx, agentHash, "", 0, now)
		if err != nil {
			return err
		}
		now, err = connectorTaskNow(tx, now)
		if err != nil {
			return err
		}
		if !credentialUntil.After(now) {
			return ErrConnectorCredential
		}
		if err := expireConnectorTasks(tx.Where("id = ? AND agent_id = ?", id, agent.ID), now); err != nil {
			return err
		}
		var row connectorTaskRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND agent_id = ?", id, agent.ID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConnectorTaskNotFound
			}
			return err
		}
		now, err = connectorTaskNow(tx, now)
		if err != nil {
			return err
		}
		if !credentialUntil.After(now) {
			return ErrConnectorCredential
		}
		if row.Status == domain.ConnectorTaskClaimed && (row.LeaseUntil == nil || !row.LeaseUntil.After(now)) {
			return ErrConnectorTaskLease
		}
		if (row.Status == domain.ConnectorTaskRunning || row.Status == domain.ConnectorTaskCancelRequested) &&
			(row.LeaseUntil == nil || !row.LeaseUntil.After(now) || (row.Status == domain.ConnectorTaskRunning && row.DeadlineAt == nil) || (row.DeadlineAt != nil && !row.DeadlineAt.After(now))) {
			if err := tx.Model(&row).Updates(map[string]any{"status": domain.ConnectorTaskRecoveryRequired, "recovery_reason": "execution_unconfirmed", "updated_at": now}).Error; err != nil {
				return err
			}
			if err := appendConnectorTaskEvent(tx, row, row.Status, domain.ConnectorTaskRecoveryRequired, "lease_expired", false, now); err != nil {
				return err
			}
			row.Status = domain.ConnectorTaskRecoveryRequired
			row.RecoveryReason = "execution_unconfirmed"
			row.UpdatedAt = now
		}
		if row.Attempt != proof.Attempt || row.LeaseHash == nil || *row.LeaseHash != domain.ConnectorTokenHash(proof.LeaseToken) {
			return ErrConnectorTaskLease
		}
		updates := map[string]any{}
		switch action {
		case "control":
		case "start":
			if row.Status == domain.ConnectorTaskRunning {
				break
			}
			if row.Status != domain.ConnectorTaskClaimed || row.LeaseUntil == nil || !row.LeaseUntil.After(now) {
				return ErrConnectorTaskLease
			}
			var input domain.ConnectorTaskRequest
			if err := json.Unmarshal(row.Input, &input); err != nil {
				return err
			}
			deadline := now.Add(time.Duration(input.TimeoutSeconds) * time.Second)
			updates = map[string]any{"status": domain.ConnectorTaskRunning, "deadline_at": deadline, "lease_until": minTaskLease(now.Add(connectorRunLeaseTTL), deadline), "updated_at": now}
		case "renew":
			if row.Status == domain.ConnectorTaskCancelRequested || row.Status == domain.ConnectorTaskRecoveryRequired {
				break
			}
			if row.Status != domain.ConnectorTaskRunning || row.LeaseUntil == nil || !row.LeaseUntil.After(now) || row.DeadlineAt == nil {
				return ErrConnectorTaskLease
			}
			updates = map[string]any{"lease_until": minTaskLease(now.Add(connectorRunLeaseTTL), *row.DeadlineAt), "updated_at": now}
		case "complete":
			if domain.ConnectorTaskTerminal(row.Status) {
				if row.OutputDigest == outputDigest {
					break
				}
				return ErrConnectorTaskLease
			}
			if row.Status != domain.ConnectorTaskRunning && row.Status != domain.ConnectorTaskCancelRequested && row.Status != domain.ConnectorTaskRecoveryRequired {
				return ErrConnectorTaskLease
			}
			if row.CancelRequested && completion.Status != domain.ConnectorTaskCanceled {
				return ErrConnectorTaskLease
			}
			updates = map[string]any{"status": completion.Status, "output": body, "output_digest": outputDigest, "updated_at": now}
		default:
			return ErrConnectorTaskLease
		}
		if len(updates) > 0 {
			if next, ok := updates["status"].(string); ok && next != row.Status {
				eventRow := row
				if until, ok := updates["lease_until"].(time.Time); ok {
					eventRow.LeaseUntil = &until
				}
				if deadline, ok := updates["deadline_at"].(time.Time); ok {
					eventRow.DeadlineAt = &deadline
				}
				if err := appendConnectorTaskEvent(tx, eventRow, row.Status, next, action, true, now); err != nil {
					return err
				}
			}
			if err := tx.Model(&row).Updates(updates).Error; err != nil {
				return err
			}
			if err := tx.Where("id = ?", row.ID).Take(&row).Error; err != nil {
				return err
			}
		}
		task, err = connectorTaskFromRow(row, action == "complete", now)
		return err
	})
	return task, err
}
func minTaskLease(until, deadline time.Time) time.Time {
	if deadline.Before(until) {
		return deadline
	}
	return until
}
