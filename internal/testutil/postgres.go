// Package testutil создаёт изолированные схемы только для тестов PostgreSQL.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	_ "github.com/lib/pq"
	"net/url"
	"os"
	"testing"
	"time"
)

func PostgresURL(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("RETRO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("RETRO_TEST_POSTGRES_URL не задан; PostgreSQL проверяется отдельным заданием CI")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("для тестов нужен PostgreSQL URL")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("не удалось открыть тестовую БД")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var b [12]byte
	if _, err = rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "retro_test_" + hex.EncodeToString(b[:])
	if _, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		db.Close()
		t.Fatal("не удалось создать изолированную тестовую схему: ", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer db.Close()
		if _, err := db.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
