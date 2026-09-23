package gen

import "testing"

// TestSnakeAcronyms pins the conversion against every acronym shape the
// catalog actually contains, plus the two cases that were already right and
// must stay right. Twelve aliases were mangled before this: IPProtocol became
// i_p_protocol on an attribute people write by hand.
func TestSnakeAcronyms(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"IPProtocol", "ip_protocol"}, {"IPProtocols", "ip_protocols"},
		{"natIP", "nat_ip"}, {"networkIP", "network_ip"},
		{"internalIPs", "internal_ips"}, {"externalIPs", "external_ips"},
		{"IPv4Range", "ipv4_range"}, {"gatewayIPv4", "gateway_ipv4"},
		{"satisfiesPZS", "satisfies_pzs"}, {"satisfiesPZI", "satisfies_pzi"},
		{"sparkRJob", "spark_r_job"}, {"mainRFileUri", "main_r_file_uri"},
		{"machineType", "machine_type"}, {"autoCreateSubnetworks", "auto_create_subnetworks"},
		{"name", "name"}, {"selfLink", "self_link"},
	} {
		if got := snake(c.in); got != c.want {
			t.Errorf("snake(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
