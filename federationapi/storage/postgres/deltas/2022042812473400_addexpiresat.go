// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deltas

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/matrix-org/dendrite/internal/sqlutil"
	"github.com/matrix-org/gomatrixserverlib"
)

func LoadAddExpiresAt(m *sqlutil.Migrations) {
	m.AddMigration(upAddexpiresat, downAddexpiresat)
}

func upAddexpiresat(tx *sql.Tx) error {
	_, err := tx.Exec("ALTER TABLE federationsender_queue_edus ADD COLUMN IF NOT EXISTS expires_at BIGINT NOT NULL DEFAULT 0;")
	if err != nil {
		return fmt.Errorf("failed to execute upgrade: %w", err)
	}
	_, err = tx.Exec("UPDATE federationsender_queue_edus SET expires_at = $1", gomatrixserverlib.AsTimestamp(time.Now().Add(time.Hour*24)))
	if err != nil {
		return fmt.Errorf("failed to update queue_edus: %w", err)
	}
	return nil
}

func downAddexpiresat(tx *sql.Tx) error {
	_, err := tx.Exec("ALTER TABLE federationsender_queue_edus DROP COLUMN expires_at;")
	if err != nil {
		return fmt.Errorf("failed to execute downgrade: %w", err)
	}
	return nil
}