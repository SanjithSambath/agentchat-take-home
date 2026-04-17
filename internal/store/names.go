package store

import (
	"fmt"

	"github.com/google/uuid"
)

// StreamNameForConversation returns the S2 stream name for a conversation.
// Kept in one place so every caller agrees on the naming convention.
// Hierarchical prefix enables enumeration via S2 list-streams.
func StreamNameForConversation(convID uuid.UUID) string {
	return fmt.Sprintf("conversations/%s", convID.String())
}
