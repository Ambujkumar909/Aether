package ledger

import (
	"fmt"
	"io"
	"strings"
)

// Human renders a byte count the way an engineer wants to read it, not the
// way a marketing page does: binary-free, unambiguous, and never rounded to
// the point where a regression could hide inside the rounding.
func Human(b int64) string {
	switch {
	case b < 1000:
		return fmt.Sprintf("%d B", b)
	case b < 1000*1000:
		return fmt.Sprintf("%.1f kB", float64(b)/1000)
	case b < 1000*1000*1000:
		return fmt.Sprintf("%.2f MB", float64(b)/1e6)
	default:
		return fmt.Sprintf("%.3f GB", float64(b)/1e9)
	}
}

// Rate renders bits per second.
func Rate(bps float64) string {
	switch {
	case bps < 1000:
		return fmt.Sprintf("%.0f bps", bps)
	case bps < 1000*1000:
		return fmt.Sprintf("%.0f kbps", bps/1000)
	default:
		return fmt.Sprintf("%.2f Mbps", bps/1e6)
	}
}

type row struct {
	label string
	a, b  int64
}

// Compare writes a side-by-side per-layer table for two breakdowns.
//
// The layout deliberately puts the removable overhead rows adjacent to the
// media payload row, because the entire argument of this system is that the
// gap between those two numbers is indefensibly large in every product
// currently shipping, and that closing it costs the user nothing.
func Compare(w io.Writer, title string, seconds float64, aName string, a Breakdown, bName string, b Breakdown) {
	rows := []row{
		{"media payload  (picture + sound)", a.MediaPayload, b.MediaPayload},
		{"RTP fixed header", a.RTPHeader, b.RTPHeader},
		{"RTP extensions", a.RTPExtension, b.RTPExtension},
		{"SRTP auth tag", a.SRTPTag, b.SRTPTag},
		{"UDP / TURN framing", a.Transport, b.Transport},
		{"IP header", a.IP, b.IP},
		{"Ethernet framing + IFG", a.Ethernet, b.Ethernet},
	}

	fmt.Fprintf(w, "\n%s\n%s\n", title, strings.Repeat("=", len(title)))
	fmt.Fprintf(w, "duration %.0f s   path: per participant, send + receive combined\n\n", seconds)
	fmt.Fprintf(w, "  %-34s %14s %14s %12s\n", "", aName, bName, "saved")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("-", 76))

	for _, r := range rows {
		saved := r.a - r.b
		pct := ""
		if r.a > 0 {
			pct = fmt.Sprintf("%+.0f%%", -100*float64(saved)/float64(r.a))
		}
		fmt.Fprintf(w, "  %-34s %14s %14s %7s %s\n",
			r.label, Human(r.a), Human(r.b), Human(saved), pct)
	}

	fmt.Fprintf(w, "  %s\n", strings.Repeat("-", 76))
	fmt.Fprintf(w, "  %-34s %14s %14s %12s\n", "TOTAL on the wire",
		Human(a.Total()), Human(b.Total()), Human(a.Total()-b.Total()))
	fmt.Fprintf(w, "  %-34s %14s %14s\n", "sustained bitrate",
		Rate(float64(a.Total())*8/seconds), Rate(float64(b.Total())*8/seconds))
	fmt.Fprintf(w, "  %-34s %14s %14s\n", "packets sent",
		fmt.Sprintf("%d", a.Packets), fmt.Sprintf("%d", b.Packets))
	fmt.Fprintf(w, "  %-34s %13.1f%% %13.1f%%\n", "payload efficiency",
		100*a.Efficiency(), 100*b.Efficiency())

	if a.Total() > 0 {
		fmt.Fprintf(w, "\n  %s is %.2fx less traffic for the same call.\n",
			bName, float64(a.Total())/float64(b.Total()))
	}
}

// Table writes a single breakdown as a standalone report.
func Table(w io.Writer, title string, b Breakdown) {
	fmt.Fprintf(w, "\n%s\n%s\n", title, strings.Repeat("=", len(title)))
	rows := []row{
		{"media payload", b.MediaPayload, 0},
		{"RTP fixed header", b.RTPHeader, 0},
		{"RTP extensions", b.RTPExtension, 0},
		{"SRTP auth tag", b.SRTPTag, 0},
		{"UDP / TURN framing", b.Transport, 0},
		{"IP header", b.IP, 0},
		{"Ethernet framing + IFG", b.Ethernet, 0},
	}
	total := b.Total()
	for _, r := range rows {
		share := 0.0
		if total > 0 {
			share = 100 * float64(r.a) / float64(total)
		}
		fmt.Fprintf(w, "  %-28s %12s  %5.1f%%\n", r.label, Human(r.a), share)
	}
	fmt.Fprintf(w, "  %-28s %12s\n", "TOTAL", Human(total))
	fmt.Fprintf(w, "  %-28s %12s\n", "bitrate", Rate(b.Bitrate()))
	fmt.Fprintf(w, "  %-28s %11.1f%%\n", "payload efficiency", 100*b.Efficiency())
}

// PacketAnatomy prints the byte-by-byte layout of a single packet. This is
// the view that makes the problem viscerally obvious to anyone who has only
// ever seen an encoder bitrate graph.
func PacketAnatomy(w io.Writer, title string, p Path, exts []RTPExtension, payload int) {
	type seg struct {
		name string
		n    int
	}
	segs := []seg{}
	if p.CountEthernet {
		segs = append(segs, seg{"Ethernet + IFG", EthernetOverhead})
	}
	if p.IPv6 {
		segs = append(segs, seg{"IPv6", IPv6Header})
	} else {
		segs = append(segs, seg{"IPv4", IPv4Header})
	}
	segs = append(segs,
		seg{p.Transport.String(), p.Transport.overhead()},
		seg{"RTP header", RTPFixedHeader},
		seg{"RTP extensions", ExtensionsSize(exts)},
		seg{"payload", payload},
		seg{"SRTP tag", p.SRTPTagBytes},
	)

	total := 0
	for _, s := range segs {
		total += s.n
	}

	fmt.Fprintf(w, "\n%s\n%s\n", title, strings.Repeat("-", len(title)))
	const width = 60
	for _, s := range segs {
		bar := int(float64(s.n) / float64(total) * width)
		if bar < 1 && s.n > 0 {
			bar = 1
		}
		ch := "#"
		if s.name == "payload" {
			ch = "*"
		}
		fmt.Fprintf(w, "  %-16s %4d B  %s\n", s.name, s.n, strings.Repeat(ch, bar))
	}
	fmt.Fprintf(w, "  %-16s %4d B   payload is %.1f%% of the packet\n",
		"TOTAL", total, 100*float64(payload)/float64(total))
}
