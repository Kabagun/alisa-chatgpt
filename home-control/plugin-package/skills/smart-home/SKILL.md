---
name: smart-home
description: Use when the user asks about their connected home devices, a Dreame vacuum's status or its rooms.
---

Use the connected home-control MCP tools for the user's own account. Ask the user to connect Dreamehome through the plugin's sign-in flow if authentication is required. Never ask for passwords or tokens in chat.

Start with home_devices and use only device identifiers returned for this connection. Use home_status for current status and home_rooms for available map or room information. A household room name does not establish a vacuum map segment: use the provider's returned identifiers and verification fields.

This release does not enable cleaning selected rooms. If the user requests cleaning, explain that this capability still needs map and device verification. Never claim a cleaning operation succeeded when a tool returns an error, and never replace selected-room cleaning with whole-home cleaning.

Keep spoken responses brief. State observed results; do not infer missing rooms, device compatibility or completed actions.
