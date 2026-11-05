// Package cleanplan constructs a DRAFT single-room plan from trusted backend
// observations. It performs no I/O and grants no authority to operate a robot.
//
// Callers must construct Observations from the authenticated catalog and the
// trusted current/saved-map reader, never from MCP/user JSON or caller-supplied
// verification claims. The repeated identities and snapshots detect accidental
// mixing; they do not authenticate the producer of these internal Go values.
//
// The fixed wire baseline is Tasshack/dreame-vacuum commit
// f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585, device.py:clean_segment/start_custom
// and types.py. Firmware acceptance remains unverified. A later dispatch layer
// must rebuild fresh observations and compare them with the concrete approved
// target/settings; this package has no dispatch or approval interface.
package cleanplan
