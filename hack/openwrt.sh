# Sourced by the e2e scripts: starts an OpenWrt image in docker as a router
# serving ubus over http, with an rpcd login for external-dns.
# shellcheck shell=bash

root_password=e2e-root-password
username=external-dns
password=e2e-password

# retry runs a command once a second until it succeeds, for up to a minute.
retry() {
	for _ in $(seq 60); do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	echo "timed out: $*" >&2
	return 1
}

login() {
	curl -sf --max-time 5 "http://$1/ubus" -H 'Content-Type: application/json' \
		-d '{"jsonrpc":"2.0","id":1,"method":"call","params":["00000000000000000000000000000000","session","login",{"username":"'"$username"'","password":"'"$password"'"}]}' |
		grep -q ubus_rpc_session
}

# start_router runs image as container name, appends it to containers for the
# caller's cleanup, and sets ip to its address.
start_router() {
	local name=$1 image=$2 prefix gateway
	docker run -d --name "$name" --privileged "$image" /sbin/init >/dev/null
	containers+=("$name")
	# Until procd is up and first boot has written the default network config,
	# reconfiguring the network fails.
	retry docker exec "$name" sh -c 'ubus call service list && uci get network.lan'

	ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name")
	prefix=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPPrefixLen}}{{end}}' "$name")
	gateway=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}' "$name")

	# A narrow ACL for external-dns, and a login for it.
	docker exec -i "$name" sh -s >/dev/null <<EOF
set -e
uci set network.lan.ipaddr='$ip/$prefix'
uci set network.lan.gateway='$gateway'
uci commit network
/etc/init.d/firewall stop
/etc/init.d/network restart
printf '%s\n%s\n' '$root_password' '$root_password' | passwd root
cat > /usr/share/rpcd/acl.d/external-dns.json <<'ACL'
{
	"external-dns": {
		"description": "external-dns: manage DNS records in dhcp",
		"read": {
			"ubus": { "uci": [ "get" ] },
			"uci": [ "dhcp" ]
		},
		"write": {
			"ubus": { "uci": [ "add", "set", "delete", "apply" ] },
			"uci": [ "dhcp" ]
		}
	}
}
ACL
uci add rpcd login
uci set rpcd.@login[-1].username='$username'
uci set rpcd.@login[-1].password="\$(uhttpd -m '$password')"
uci add_list rpcd.@login[-1].read='external-dns'
uci add_list rpcd.@login[-1].write='external-dns'
uci commit rpcd
/etc/init.d/rpcd restart
EOF
	retry login "$ip"
}
