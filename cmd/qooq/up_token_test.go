package main

import (
	"reflect"
	"testing"
)

func TestInheritedTokenEnv(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	cases := []struct {
		name string
		d    daemon
		kv   map[string]string
		want []string
	}{
		{"watcher gets qooqd's token", watcherDaemon(), map[string]string{"QOOQ_TOKEN": "s3cret"},
			[]string{"QOOQ_WATCHER_QOOQD_TOKEN=s3cret"}},
		{"explicit watcher token wins", watcherDaemon(), map[string]string{"QOOQ_TOKEN": "s3cret", "QOOQ_WATCHER_QOOQD_TOKEN": "other"}, nil},
		{"no token, nothing added", watcherDaemon(), map[string]string{}, nil},
		{"qooqd untouched", qooqdDaemon(nil), map[string]string{"QOOQ_TOKEN": "s3cret"}, nil},
	}
	for _, c := range cases {
		if got := inheritedTokenEnv(c.d, env(c.kv)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
