package main

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// compatDB loads only the pg_tde_compat statements from anydbver_version.sql,
// so the test follows the shipped table.
func compatDB(t *testing.T) *sql.DB {
	t.Helper()
	data, err := os.ReadFile("../../anydbver_version.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "CREATE TABLE pg_tde_compat") || strings.HasPrefix(line, "INSERT INTO pg_tde_compat") {
			if _, err := db.Exec(line); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func TestPickPgTdePair(t *testing.T) {
	db := compatDB(t)
	defer db.Close()
	cases := []struct {
		major            int
		ppg, tde         string
		wantPpg, wantTde string
	}{
		{17, "17", "", "17.11-1", "2.2.2-1"},
		{17, "latest", "latest", "17.11-1", "2.2.2-1"},
		{17, "17", "2.1.2", "17.10-1", "2.1.2-1"},
		{17, "17", "2.2.0", "17.10-1", "2.2.0-1"},
		{17, "17.9", "", "17.9-1", "2.1.2-1"},
		{17, "17.10", "", "17.10-2", "2.2.2-1"},
		{17, "17.10-1", "", "17.10-1", "2.2.0-1"},
		{17, "17", "2.2", "17.11-1", "2.2.2-1"},
		{18, "18", "2.2.1", "18.6-1", "2.2.1-2"},
		{18, "18.4-1", "2.2.1", "18.4-1", "2.2.1-1"},
		{17, "17.11", "2.1.2", "", ""},
		{17, "17.1", "", "", ""},
	}
	for _, c := range cases {
		gotPpg, gotTde, _ := pickPgTdePair(db, c.major, c.ppg, c.tde)
		if gotPpg != c.wantPpg || gotTde != c.wantTde {
			t.Errorf("ppg:%s pg-tde:%s = %q %q, want %q %q", c.ppg, c.tde, gotPpg, gotTde, c.wantPpg, c.wantTde)
		}
	}
}

func TestSetItemVersion(t *testing.T) {
	cases := map[string]string{
		"ppg:17,master=node1":          "ppg:17.11-1,master=node1",
		"ppg":                          "ppg:17.11-1",
		"ppg:latest":                   "ppg:17.11-1",
		"pg-tde:vault=node0":           "pg-tde:17.11-1,vault=node0",
		"pg-tde:2.2,vault=node0,key=k": "pg-tde:17.11-1,vault=node0,key=k",
	}
	for in, want := range cases {
		if got := setItemVersion(in, "17.11-1"); got != want {
			t.Errorf("setItemVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
