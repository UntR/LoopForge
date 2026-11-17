package repo

import (
	"errors"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

var ErrConnectorCredential = errors.New("connector credential unavailable")
var ErrConnectorNotFound = errors.New("connector not found")

type connectorCodeRow struct {
	Hash       string `gorm:"primaryKey;size:64"`
	OwnerID    int64  `gorm:"index"`
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

func (connectorCodeRow) TableName() string { return "connector_bind_codes" }

type connectorDeviceRow struct {
	ID        string `gorm:"primaryKey;size:64"`
	OwnerID   int64  `gorm:"index"`
	TokenHash string `gorm:"uniqueIndex;size:64"`
	Name      string `gorm:"size:128"`
	OS        string `gorm:"size:16"`
	Arch      string `gorm:"size:16"`
	Revoked   bool
	ExpiresAt time.Time
	CreatedAt time.Time
}

func (connectorDeviceRow) TableName() string { return "connector_devices" }

type connectorAgentRow struct {
	ID        string `gorm:"primaryKey;size:64"`
	DeviceID  string `gorm:"uniqueIndex:idx_connector_device_provider;size:64"`
	Provider  string `gorm:"uniqueIndex:idx_connector_device_provider;size:32"`
	TokenHash string `gorm:"uniqueIndex;size:64"`
	Version   string `gorm:"size:256"`
	Revoked   bool
	CreatedAt time.Time
}

func (connectorAgentRow) TableName() string { return "connector_agents" }

func connectorDevice(row connectorDeviceRow) domain.ConnectorDevice {
	return domain.ConnectorDevice{ID: row.ID, Name: row.Name, OS: row.OS, Arch: row.Arch, Revoked: row.Revoked, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt}
}
func connectorAgent(row connectorAgentRow) domain.ConnectorAgent {
	return domain.ConnectorAgent{ID: row.ID, DeviceID: row.DeviceID, Provider: row.Provider, Version: row.Version, Revoked: row.Revoked, CreatedAt: row.CreatedAt}
}

func (r *ConnectorsRepo) IssueConnectorCode(ownerID int64, hash string, expires time.Time) error {
	if ownerID <= 0 || len(hash) != 64 {
		return ErrConnectorCredential
	}
	return r.DB.Create(&connectorCodeRow{Hash: hash, OwnerID: ownerID, ExpiresAt: expires}).Error
}

// Consume and identity creation commit together. A failed enrollment never
// burns the code, and concurrent attempts cannot both become owners.
func (r *ConnectorsRepo) EnrollConnector(input domain.ConnectorEnrollment, deviceID string, agentIDs []string, now time.Time) (domain.ConnectorIdentity, error) {
	var result domain.ConnectorIdentity
	if err := domain.ValidateConnectorEnrollment(input); err != nil || len(agentIDs) != len(input.Agents) || deviceID == "" {
		return result, ErrConnectorCredential
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		hash := domain.ConnectorTokenHash(input.Code)
		claimed := tx.Model(&connectorCodeRow{}).Where("hash = ? AND consumed_at IS NULL AND expires_at > ?", hash, now).Update("consumed_at", now)
		if claimed.Error != nil {
			return claimed.Error
		}
		if claimed.RowsAffected != 1 {
			return ErrConnectorCredential
		}
		var code connectorCodeRow
		if err := tx.Where("hash = ?", hash).Take(&code).Error; err != nil {
			return err
		}
		device := connectorDeviceRow{ID: deviceID, OwnerID: code.OwnerID, TokenHash: domain.ConnectorTokenHash(input.Credential), Name: input.Name, OS: input.OS, Arch: input.Arch, ExpiresAt: now.Add(30 * 24 * time.Hour), CreatedAt: now}
		if err := tx.Create(&device).Error; err != nil {
			return err
		}
		result.Device = connectorDevice(device)
		result.Agents = []domain.ConnectorAgent{}
		for i, agent := range input.Agents {
			row := connectorAgentRow{ID: agentIDs[i], DeviceID: deviceID, Provider: agent.Provider, TokenHash: domain.ConnectorTokenHash(agent.Credential), Version: agent.Version, CreatedAt: now}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			result.Agents = append(result.Agents, connectorAgent(row))
		}
		return nil
	})
	if err != nil {
		return domain.ConnectorIdentity{}, err
	}
	return result, nil
}

func (r *ConnectorsRepo) ConnectorIdentityByHash(hash string, now time.Time) (domain.ConnectorIdentity, error) {
	var device connectorDeviceRow
	if err := r.DB.Where("token_hash = ? AND revoked = ? AND expires_at > ?", hash, false, now).Take(&device).Error; err != nil {
		return domain.ConnectorIdentity{}, ErrConnectorCredential
	}
	return connectorIdentity(r.DB, device)
}

func connectorIdentity(db *gorm.DB, device connectorDeviceRow) (domain.ConnectorIdentity, error) {
	var rows []connectorAgentRow
	if err := db.Where("device_id = ?", device.ID).Order("provider ASC").Find(&rows).Error; err != nil {
		return domain.ConnectorIdentity{}, err
	}
	result := domain.ConnectorIdentity{Device: connectorDevice(device), Agents: []domain.ConnectorAgent{}}
	for _, row := range rows {
		result.Agents = append(result.Agents, connectorAgent(row))
	}
	return result, nil
}

func (r *ConnectorsRepo) ConnectorAgentByHash(hash string, now time.Time) (domain.ConnectorAgent, error) {
	var row connectorAgentRow
	query := r.DB.Model(&connectorAgentRow{}).Select("connector_agents.*").Joins("JOIN connector_devices ON connector_devices.id = connector_agents.device_id").Where("connector_agents.token_hash = ? AND connector_agents.revoked = ? AND connector_devices.revoked = ? AND connector_devices.expires_at > ?", hash, false, false, now)
	if err := query.Take(&row).Error; err != nil {
		return domain.ConnectorAgent{}, ErrConnectorCredential
	}
	return connectorAgent(row), nil
}

func (r *ConnectorsRepo) ListConnectors(ownerID int64) ([]domain.ConnectorIdentity, error) {
	var rows []connectorDeviceRow
	if err := r.DB.Where("owner_id = ?", ownerID).Order("created_at DESC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := []domain.ConnectorIdentity{}
	for _, row := range rows {
		identity, err := connectorIdentity(r.DB, row)
		if err != nil {
			return nil, err
		}
		result = append(result, identity)
	}
	return result, nil
}

func (r *ConnectorsRepo) RevokeConnectorDevice(ownerID int64, id string) error {
	result := r.DB.Model(&connectorDeviceRow{}).Where("id = ? AND owner_id = ?", id, ownerID).Update("revoked", true)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.DB.Model(&connectorDeviceRow{}).Where("id = ? AND owner_id = ?", id, ownerID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrConnectorNotFound
		}
	}
	return nil
}

func (r *ConnectorsRepo) RevokeConnectorAgent(ownerID int64, id string) error {
	subquery := r.DB.Model(&connectorDeviceRow{}).Select("id").Where("owner_id = ?", ownerID)
	var row connectorAgentRow
	if err := r.DB.Where("id = ? AND device_id IN (?)", id, subquery).Take(&row).Error; err != nil {
		return ErrConnectorNotFound
	}
	return r.DB.Model(&connectorAgentRow{}).Where("id = ?", row.ID).Update("revoked", true).Error
}
