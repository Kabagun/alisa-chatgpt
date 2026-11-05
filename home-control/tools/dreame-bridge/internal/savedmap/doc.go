// Package savedmap reads bounded Dreame saved-map metadata without network or
// filesystem access. Its values are private application data: callers must not
// log entries, references, decoded room names, or frame identifiers.
//
// Decoding a saved map does not establish the robot's current floor and provides
// no authorization to control it. Callers must reconcile selected and current
// saved-map identifiers separately and retain read-only behavior.
//
// The format logic is adapted from Tasshack/dreame-vacuum, commit
// f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585, map.py and types.py.
// Copyright (c) 2022 Tasshack. SPDX-License-Identifier: MIT.
// The full upstream notice is retained in ../../LICENSE.upstream.
package savedmap
