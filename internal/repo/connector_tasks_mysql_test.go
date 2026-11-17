package repo

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Explicit opt-in, literal loopback and a dedicated fixture database guard
// schema creation. Never consume the application's production DSN here.
func connectorMySQLFixture(t *testing.T) *Store {
	t.Helper()
	raw := os.Getenv("LOOPFORGE_CONNECTOR_MYSQL_TEST_DSN")
	if raw == "" {
		t.Skip("isolated MySQL connector fixture not configured")
	}
	configuration, err := mysqlDriver.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid isolated MySQL fixture DSN")
	}
	host, _, err := net.SplitHostPort(configuration.Addr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || configuration.Net != "tcp" || configuration.DBName != "connector_fixture" || os.Getenv("LOOPFORGE_CONNECTOR_MYSQL_TEST_ALLOW_CREATE") != "1" {
		t.Fatal("MySQL tests require explicit loopback connector_fixture permission")
	}
	configuration.ParseTime = true
	configuration.Loc = time.UTC
	configuration.MultiStatements = true
	configuration.Timeout = 5 * time.Second
	configuration.ReadTimeout = 15 * time.Second
	configuration.WriteTimeout = 15 * time.Second
	admin, err := sql.Open("mysql", configuration.FormatDSN())
	if err != nil {
		t.Fatal("cannot open isolated fixture")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "lf_connector_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		admin.Close()
		t.Fatal("cannot create isolated connector schema", err)
	}
	var pool *sql.DB
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
		admin.Close()
	})
	configuration.DBName = schema
	pool, err = sql.Open("mysql", configuration.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(8)
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	// Test the reviewed manual DDL, not an AutoMigrate that could conceal a
	// mismatch between shipped SQL and the repository models.
	for _, name := range []string{"033_connector_identities.sql", "034_connector_tasks.sql"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(body)).Error; err != nil {
			t.Fatal("manual connector schema failed", err)
		}
	}
	return newStore(db)
}

func TestConnectorTasksMySQLClockLocksAndRecovery(t *testing.T) {
	s := connectorMySQLFixture(t)
	now := time.Now().UTC()
	input := connectorFixture()
	issueConnectorFixture(t, s, input, 42, now)
	identity, err := s.EnrollConnector(input, "mysql-device", []string{"mysql-a", "mysql-b", "mysql-c"}, now)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.ConnectorTokenHash(input.Agents[0].Credential)
	wrongCase := typedTask(strings.ToUpper(identity.Agents[0].ID), "wrong-case-agent")
	if _, _, err := s.SubmitConnectorTask(42, "wrong-case-agent", wrongCase, now); !errors.Is(err, ErrConnectorTaskNotFound) {
		t.Fatal("MySQL accepted a differently cased Agent identity", err)
	}
	request := typedTask(identity.Agents[0].ID, "SameRequest")
	var group sync.WaitGroup
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	for _, id := range []string{"mysql-first", "mysql-retry"} {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			task, _, err := s.SubmitConnectorTask(42, id, request, now.Add(48*time.Hour))
			ids <- task.ID
			errs <- err
		}(id)
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("MySQL concurrent submit", err)
		}
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatal("MySQL duplicated idempotent request")
		}
	}
	lower := request
	lower.IdempotencyKey = "samerequest"
	if _, created, err := s.SubmitConnectorTask(42, "mysql-lower", lower, now); err != nil || !created {
		t.Fatal("MySQL collation collapsed distinct request keys", err)
	}
	claims := make(chan *domain.ConnectorTask, 2)
	errs = make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			task, err := s.ClaimConnectorTask(hash, taskLease(byte(80+i)), now.Add(-48*time.Hour))
			claims <- task
			errs <- err
		}(i)
	}
	group.Wait()
	close(claims)
	close(errs)
	won, busy := 0, 0
	for err := range errs {
		if err == nil {
			won++
		} else if errors.Is(err, ErrConnectorAgentBusy) {
			busy++
		} else {
			t.Fatal("MySQL concurrent claim", err)
		}
	}
	if won != 1 || busy != 1 {
		t.Fatalf("MySQL claims won=%d busy=%d", won, busy)
	}
	var claimed *domain.ConnectorTask
	for task := range claims {
		if task != nil {
			claimed = task
		}
	}
	if claimed == nil || claimed.RunAllowed || claimed.LeaseUntil == nil {
		t.Fatal("invalid MySQL claim")
	}
	var row connectorTaskRow
	if err := s.DB.Where("id = ?", claimed.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	var databaseNow time.Time
	if err := s.DB.Raw("SELECT UTC_TIMESTAMP(3)").Scan(&databaseNow).Error; err != nil {
		t.Fatal(err)
	}
	if difference := claimed.LeaseUntil.Sub(databaseNow); difference < 25*time.Second || difference > 31*time.Second {
		t.Fatal("claim lease used the caller's skewed clock", difference)
	}
	var lease string
	for _, value := range []byte{80, 81} {
		candidate := taskLease(value)
		if row.LeaseHash != nil && *row.LeaseHash == domain.ConnectorTokenHash(candidate) {
			lease = candidate
		}
	}
	proof := domain.ConnectorTaskLease{Attempt: claimed.Attempt, LeaseToken: lease}
	running := actTypedTask(t, s, hash, claimed.ID, "start", proof, nil, now.Add(48*time.Hour))
	if !running.RunAllowed || running.LeaseRemainingMillis > 45000 || running.LeaseRemainingMillis < 44000 {
		t.Fatal("MySQL start grant not bounded")
	}
	if err := s.DB.Exec("UPDATE connector_tasks SET lease_until = UTC_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?", claimed.ID).Error; err != nil {
		t.Fatal(err)
	}
	unknown, err := s.ConnectorTask(42, claimed.ID, now.Add(-48*time.Hour))
	if err != nil || unknown.Status != domain.ConnectorTaskRecoveryRequired || unknown.RunAllowed {
		t.Fatal("MySQL lost running lease was not fenced", err)
	}
	if _, err := s.ClaimConnectorTask(hash, taskLease(82), now); !errors.Is(err, ErrConnectorAgentBusy) {
		t.Fatal("MySQL blindly reran unknown execution", err)
	}
	result := domain.ConnectorTaskResult{Status: domain.ConnectorTaskFailed, Text: "synthetic stopped worker", Quiesced: true}
	actTypedTask(t, s, hash, claimed.ID, "complete", proof, &result, now)
	events, err := s.ConnectorTaskEvents(42, claimed.ID, 0)
	if err != nil || len(events) != 5 {
		t.Fatal("MySQL audit not atomic with state", err, len(events))
	}
}
