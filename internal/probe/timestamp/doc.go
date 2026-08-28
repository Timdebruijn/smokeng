// Package timestamp will isolate packet timestamping (DESIGN.md §5.2): on
// Linux, SO_TIMESTAMPING with software TX timestamps read back from the
// socket error queue (MSG_ERRQUEUE) and RX timestamps from control messages;
// elsewhere, a userspace-clock fallback that sets the degradation flags
// (store.FlagUserspaceTX / FlagUserspaceRX) so reduced accuracy is recorded
// per measurement, never silent.
//
// Status: not implemented. This is deliberately the smallest, most-tested
// package in the prober; its interface is defined together with the socket
// loop in the ICMP engine step, not speculatively here.
package timestamp
