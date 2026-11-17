package repo

import (
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

type connectorTaskEventRow struct {
	ID         int64  `gorm:"primaryKey;autoIncrement;index:idx_connector_task_events,priority:2"`
	TaskID     string `gorm:"index:idx_connector_task_events,priority:1;size:64"`
	Attempt    int
	Action     string `gorm:"size:32"`
	FromStatus string `gorm:"size:32"`
	ToStatus   string `gorm:"size:32"`
	AgentID    string `gorm:"size:64"`
	OwnerID    int64
	LeaseUntil *time.Time
	DeadlineAt *time.Time
	CreatedAt  time.Time
}

func (connectorTaskEventRow) TableName() string { return "connector_task_events" }

// State events contain identifiers and transitions, never prompts, output,
// local paths, credentials or lease proofs. Insert in the state transaction.
func appendConnectorTaskEvent(tx *gorm.DB, row connectorTaskRow, from, to, action string, byAgent bool, now time.Time) error {
	event := connectorTaskEventRow{TaskID: row.ID, Attempt: row.Attempt, Action: action, FromStatus: from, ToStatus: to, LeaseUntil: row.LeaseUntil, DeadlineAt: row.DeadlineAt, CreatedAt: now}
	if byAgent {
		event.AgentID = row.AgentID
	} else if action == "submitted" || action == "cancel_requested" {
		event.OwnerID = row.OwnerID
	}
	return tx.Create(&event).Error
}
func (r *ConnectorTasksRepo) ConnectorTaskEvents(ownerID int64, id string, after int64) ([]domain.ConnectorTaskEvent, error) {
	if after < 0 {
		return nil, ErrConnectorTaskNotFound
	}
	var count int64
	if err := r.DB.Model(&connectorTaskRow{}).Where("owner_id = ? AND id = ?", ownerID, id).Count(&count).Error; err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, ErrConnectorTaskNotFound
	}
	var rows []connectorTaskEventRow
	if err := r.DB.Where("task_id = ? AND id > ?", id, after).Order("id ASC").Limit(1000).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []domain.ConnectorTaskEvent{}
	for _, row := range rows {
		out = append(out, domain.ConnectorTaskEvent{ID: row.ID, TaskID: row.TaskID, Attempt: row.Attempt, Action: row.Action, FromStatus: row.FromStatus, ToStatus: row.ToStatus, AgentID: row.AgentID, OwnerID: row.OwnerID, LeaseUntil: row.LeaseUntil, DeadlineAt: row.DeadlineAt, CreatedAt: row.CreatedAt})
	}
	return out, nil
}
