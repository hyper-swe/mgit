package guestnet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestLocalhostEntry_IsTheNameTableLineNotAMention pins what "the guest maps
// localhost" means when the table is read directly: a non-comment line whose
// hostname fields include localhost. A comment that merely mentions the word
// is exactly the false pass a resolver-free check must not give.
// Refs: MGIT-169, MGIT-159
func TestLocalhostEntry_IsTheNameTableLineNotAMention(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"the_ipv4_loopback_line", "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost\n", "127.0.0.1\tlocalhost"},
		{"an_ipv6_only_table_still_maps_it", "::1\tlocalhost ip6-localhost ip6-loopback\n", "::1\tlocalhost ip6-localhost ip6-loopback"},
		{"a_comment_mentioning_localhost_maps_nothing", "# localhost is resolved by DNS here\n10.0.0.1\tgateway\n", ""},
		{"an_address_column_is_not_a_hostname", "localhost\t127.0.0.1\n", ""},
		{"an_empty_table_maps_nothing", "", ""},
		{"whitespace_and_a_later_entry", "\n\n  10.0.0.5  build-cache \n127.0.0.1 localhost # loopback\n", "127.0.0.1 localhost # loopback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LocalhostEntry(tt.body))
		})
	}
}
