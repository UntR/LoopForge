package repo

import (
	"errors"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The caller's transaction locks the Agent row, serializing claim decisions.
// Parent revocation/expiry and owner scope are rechecked on every operation.
func (r *ConnectorsRepo) connectorTaskPrincipal(tx *gorm.DB, hash, id string, ownerID int64, now time.Time) (domain.ConnectorAgent, time.Time, error) {
	var row connectorAgentRow
	query := tx.Model(&connectorAgentRow{}).Select("connector_agents.*").Clauses(clause.Locking{Strength: "UPDATE"}).Joins("JOIN connector_devices ON connector_devices.id = connector_agents.device_id").Where("connector_agents.revoked = ? AND connector_devices.revoked = ? AND connector_devices.expires_at > ?", false, false, now)
	if hash != "" {
		query = query.Where("connector_agents.token_hash = ?", hash)
	} else {
		query = query.Where("connector_agents.id = ? AND connector_devices.owner_id = ?", id, ownerID)
	}
	if err := query.Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.ConnectorAgent{}, time.Time{}, ErrConnectorCredential
		}
		return domain.ConnectorAgent{}, time.Time{}, err
	}
	if hash == "" && row.ID != id {
		return domain.ConnectorAgent{}, time.Time{}, ErrConnectorCredential
	}
	var device connectorDeviceRow
	if err := tx.Where("id = ?", row.DeviceID).Take(&device).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.ConnectorAgent{}, time.Time{}, ErrConnectorCredential
		}
		return domain.ConnectorAgent{}, time.Time{}, err
	}
	return connectorAgent(row), device.ExpiresAt, nil
}

// Reject unknown credentials before a machine handler reads a large payload.
// The task transaction still authenticates again to fence concurrent revokes.
func (r *ConnectorsRepo) ValidateConnectorTaskCredential(hash string, clock time.Time) error {
	now, err := connectorTaskNow(r.DB, clock)
	if err != nil {
		return err
	}
	var count int64
	err = r.DB.Model(&connectorAgentRow{}).Joins("JOIN connector_devices ON connector_devices.id = connector_agents.device_id").Where("connector_agents.token_hash = ? AND connector_agents.revoked = ? AND connector_devices.revoked = ? AND connector_devices.expires_at > ?", hash, false, false, now).Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConnectorCredential
	}
	return nil
}
