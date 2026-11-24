package main

import "testing"

func TestQuoteDecimalInventoryValues(t *testing.T) {
	cases := map[string]string{
		"extra_mongot_version='1.70'":       `extra_mongot_version="'1.70'"`,
		"extra_clickhouse_version='26.10'":  `extra_clickhouse_version="'26.10'"`,
		"extra_psmdb_version='8.0'":         `extra_psmdb_version="'8.0'"`,
		"extra_postgresql_version='17'":     "extra_postgresql_version='17'",
		"extra_pbm_version='2.17.0-1'":      "extra_pbm_version='2.17.0-1'",
		"extra_mysql_version='8.4.5'":       "extra_mysql_version='8.4.5'",
		"extra_master_ip='172.18.0.2'":      "extra_master_ip='172.18.0.2'",
		"extra_db_password='secret'":        "extra_db_password='secret'",
		"ansible_ssh_common_args='-o x=1' ": "ansible_ssh_common_args='-o x=1' ",
	}
	for in, want := range cases {
		if got := quoteDecimalInventoryValues(in); got != want {
			t.Errorf("quoteDecimalInventoryValues(%q) = %q, want %q", in, got, want)
		}
	}
}
