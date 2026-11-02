// Command bitbudget prints the bit-level accounting for a meeting, layer by
// layer, against a mainstream baseline.
//
// This tool exists so that no claim in this project is ever made without the
// arithmetic that supports it being one command away from being checked.
//
//	go run ./cmd/bitbudget -n 4 -minutes 15
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/aether-rtc/aether/internal/ledger"
)

func main() {
	n := flag.Int("n", 4, "participants in the meeting")
	minutes := flag.Float64("minutes", 15, "meeting duration")
	events := flag.Float64("events", 40, "control-plane events per minute across the room")
	flag.Parse()

	seconds := *minutes * 60
	out := os.Stdout

	base := ledger.BaselineScenario(*n, seconds).Project()
	ours := ledger.AetherScenario(*n, seconds).Project()

	ledger.Compare(out,
		fmt.Sprintf("MEDIA PLANE - %d PARTICIPANTS - %.0f MINUTES", *n, *minutes),
		seconds, "baseline", base, "aether", ours)

	// ----------------------------------------------------------------- //

	bs := ledger.BaselineSignal(*events)
	as := ledger.AetherSignal(*events)
	bsb, asb := bs.Bytes(seconds), as.Bytes(seconds)

	fmt.Fprintf(out, "\nCONTROL PLANE - %.0f events/min for %.0f minutes\n", *events, *minutes)
	fmt.Fprintf(out, "%s\n", "===============================================")
	fmt.Fprintf(out, "  %-44s %12s\n", bs.Name, ledger.Human(bsb))
	fmt.Fprintf(out, "  %-44s %12s\n", as.Name, ledger.Human(asb))
	if asb > 0 {
		fmt.Fprintf(out, "  %-44s %11.1fx\n", "reduction", float64(bsb)/float64(asb))
	}

	// ----------------------------------------------------------------- //

	p := ledger.DefaultPath()
	ledger.PacketAnatomy(out,
		"ANATOMY OF ONE 20 ms OPUS PACKET AT 32 kbps",
		p, ledger.StandardExtensions, 80)

	var reduced []ledger.RTPExtension
	for _, e := range ledger.StandardExtensions {
		if !e.Droppable {
			reduced = append(reduced, e)
		}
	}
	ledger.PacketAnatomy(out,
		"THE SAME AUDIO AT 60 ms WITH THE REDUNDANT EXTENSIONS DROPPED",
		p, reduced, 240)

	fmt.Fprintf(out, "\n  Identical codec, identical bitrate, identical audio.\n")
	fmt.Fprintf(out, "  Three packets became one, and the header rode once instead of thrice.\n")

	// ----------------------------------------------------------------- //

	fmt.Fprintf(out, "\nPER-PACKET OVERHEAD BY TRANSPORT (full extension set)\n")
	fmt.Fprintf(out, "%s\n", "=====================================================")
	for _, t := range []ledger.Transport{
		ledger.TransportUDP, ledger.TransportTCP,
		ledger.TransportTURNChannel, ledger.TransportTURNIndication,
	} {
		q := p
		q.Transport = t
		fmt.Fprintf(out, "  %-20s %3d B/packet   %s at 50 pps\n",
			t.String(), q.PerPacketOverhead(ledger.StandardExtensions),
			ledger.Rate(float64(q.PerPacketOverhead(ledger.StandardExtensions))*8*50))
	}
	fmt.Fprintf(out, "\n  Dropping the redundant RTP extensions once SSRCs are bound saves\n")
	fmt.Fprintf(out, "  %d bytes on every packet, which is %s on one 50 pps audio stream.\n",
		ledger.DroppableExtensionsSize(),
		ledger.Rate(float64(ledger.DroppableExtensionsSize())*8*50))

	fmt.Fprintf(out, "\n  Effective encoder MTU payload, full extensions: %d B\n",
		ledger.EffectiveMTUPayload(p, ledger.StandardExtensions, 1500))
	fmt.Fprintf(out, "  Effective encoder MTU payload, reduced:        %d B\n",
		ledger.EffectiveMTUPayload(p, reduced, 1500))
	fmt.Fprintln(out)
}
