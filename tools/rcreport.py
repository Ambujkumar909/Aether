"""Account for every byte of a captured remote-control session.

Reads the tshark field dump produced by rccheck.sh and adds up what is
genuinely on the wire, layer by layer. Nothing here is modelled: link, network
and transport header sizes come from the capture, because the assumption is
usually wrong. Loopback on this machine is IPv4 when the listener binds
127.0.0.1 and IPv6 when it binds ::1, and those differ by 20 bytes per packet.
"""

import json
import sys


def to_int(s):
    if not s:
        return 0
    # tshark emits comma-joined values when a field appears more than once in
    # a packet, which happens for coalesced WebSocket frames.
    total = 0
    for part in str(s).split(","):
        part = part.strip()
        if part:
            try:
                total += int(part)
            except ValueError:
                pass
    return total


def main():
    fields_path, app_path, seconds, mode = sys.argv[1:5]
    seconds = int(seconds)

    with open(app_path, encoding="utf-8") as fh:
        app = json.load(fh)

    packets = 0
    wire_bytes = 0
    ip_hdr = 0
    tcp_hdr = 0
    tcp_payload = 0
    ws_payload = 0
    data_packets = 0

    with open(fields_path, encoding="utf-8") as fh:
        for line in fh:
            cols = line.rstrip("\n").split(",")
            # The websocket field can itself contain commas, so re-split by
            # taking the first five columns positionally and joining the rest.
            while len(cols) < 6:
                cols.append("")
            frame_len = to_int(cols[0])
            iph = to_int(cols[1])
            ip6_plen = to_int(cols[2])
            tcph = to_int(cols[3])
            tcpl = to_int(cols[4])
            wsl = to_int(",".join(cols[5:]))

            if frame_len == 0:
                continue
            packets += 1
            wire_bytes += frame_len
            # IPv6 reports payload length rather than header length; the
            # fixed header is 40 bytes.
            ip_hdr += iph if iph else (40 if ip6_plen else 0)
            tcp_hdr += tcph
            tcp_payload += tcpl
            ws_payload += wsl
            if tcpl > 0:
                data_packets += 1

    link = wire_bytes - ip_hdr - tcp_hdr - tcp_payload
    ws_framing = tcp_payload - ws_payload
    app_payload = app["payloadBytes"]
    events = app["events"]
    messages = app["messages"]

    def pct(n):
        return 100.0 * n / wire_bytes if wire_bytes else 0.0

    print()
    print("  REMOTE CONTROL INPUT, MEASURED ON THE WIRE")
    print("  " + "-" * 62)
    print(f"  mode                      {mode}")
    print(f"  duration                  {seconds} s")
    print(f"  packets captured          {packets}  ({data_packets} carrying data)")
    print()
    print("  LAYER BY LAYER")
    print(f"  link framing              {link:8d} B   {pct(link):5.1f}%")
    print(f"  IP headers                {ip_hdr:8d} B   {pct(ip_hdr):5.1f}%")
    print(f"  TCP headers               {tcp_hdr:8d} B   {pct(tcp_hdr):5.1f}%")
    print(f"  WebSocket framing         {ws_framing:8d} B   {pct(ws_framing):5.1f}%")
    print(f"  input payload             {ws_payload:8d} B   {pct(ws_payload):5.1f}%")
    print(f"  {'total on the wire':<24}  {wire_bytes:8d} B   100.0%")
    print()
    print("  APPLICATION VIEW")
    print(f"  input events              {events}")
    print(f"  websocket messages        {messages}")
    print(f"  payload the app wrote     {app_payload} B")
    if events:
        print(f"  bytes per event, payload  {app_payload / events:.2f} B")
        print(f"  bytes per event, on wire  {wire_bytes / events:.2f} B")
    if messages:
        print(f"  events per message        {events / messages:.2f}")
    print()
    print("  RATES")
    print(f"  payload                   {app_payload * 8 / seconds / 1000:.2f} kbit/s")
    print(f"  on the wire               {wire_bytes * 8 / seconds / 1000:.2f} kbit/s")
    print()
    eff = pct(ws_payload)
    print(f"  PAYLOAD EFFICIENCY        {eff:.1f}%")
    print("  " + "-" * 62)
    print()


if __name__ == "__main__":
    main()
