package main

import "testing"

func TestResolveSetup(t *testing.T) {
	current := &GuildConfig{GuildID: "g", ChannelID: "old-chan", PingRoleID: "old-role"}

	tests := []struct {
		name        string
		existing    *GuildConfig
		in          setupInput
		wantChannel string
		wantPing    string
		wantErr     bool
	}{
		{
			name:        "first run stores channel and role",
			in:          setupInput{ChannelID: "chan", PingRoleID: "role", PingProvided: true},
			wantChannel: "chan",
			wantPing:    "role",
		},
		{
			name:    "first run without a channel is rejected",
			in:      setupInput{PingRoleID: "role", PingProvided: true},
			wantErr: true,
		},
		{
			name:        "re-run edits the channel and keeps the role",
			existing:    current,
			in:          setupInput{ChannelID: "new-chan"},
			wantChannel: "new-chan",
			wantPing:    "old-role",
		},
		{
			name:        "re-run edits the role and keeps the channel",
			existing:    current,
			in:          setupInput{PingRoleID: "new-role", PingProvided: true},
			wantChannel: "old-chan",
			wantPing:    "new-role",
		},
		{
			name:        "remove_ping_role clears the role",
			existing:    current,
			in:          setupInput{RemovePing: true},
			wantChannel: "old-chan",
			wantPing:    "",
		},
		{
			name:     "setting and removing the role at once is rejected",
			existing: current,
			in:       setupInput{PingRoleID: "new-role", PingProvided: true, RemovePing: true},
			wantErr:  true,
		},
		{
			name:        "re-run with no options keeps everything",
			existing:    current,
			in:          setupInput{},
			wantChannel: "old-chan",
			wantPing:    "old-role",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			channel, ping, err := resolveSetup(tc.existing, tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveSetup() = (%q, %q, nil), want error", channel, ping)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSetup() error = %v", err)
			}
			if channel != tc.wantChannel {
				t.Errorf("channel = %q, want %q", channel, tc.wantChannel)
			}
			if ping != tc.wantPing {
				t.Errorf("ping role = %q, want %q", ping, tc.wantPing)
			}
		})
	}
}

// Editing one server must never touch another server's configuration.
func TestSetGuildConfigEditIsPerGuild(t *testing.T) {
	store, err := openStore(t.TempDir() + "/bot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SetGuildConfig("guild-1", "chan-1", "role-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGuildConfig("guild-2", "chan-2", ""); err != nil {
		t.Fatal(err)
	}

	// Re-running /setup in guild-1 overwrites only guild-1.
	if err := store.SetGuildConfig("guild-1", "chan-1-fixed", ""); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetGuildConfig("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ChannelID != "chan-1-fixed" || got.PingRoleID != "" {
		t.Errorf("guild-1 = %+v, want channel chan-1-fixed with no role", got)
	}

	other, err := store.GetGuildConfig("guild-2")
	if err != nil {
		t.Fatal(err)
	}
	if other.ChannelID != "chan-2" {
		t.Errorf("guild-2 channel = %q, want chan-2 (unaffected)", other.ChannelID)
	}
}
