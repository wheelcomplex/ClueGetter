// GlueGetter - Does things with mail
//
// Copyright 2015 Dolf Schimmel, Freeaqingme.
//
// This Source Code Form is subject to the terms of the two-clause BSD license.
// For its contents, please refer to the LICENSE file.
//
package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

var greylistOncePrepStmt = *new(sync.Once)
var greylistGetRecentVerdictsStmt = *new(*sql.Stmt)

type greylistVerdict struct {
	verdict string
	date    *time.Time
}

func greylistPrepStmt() {
	_, tzOffset := time.Now().Local().Zone()
	stmt, err := Rdbms.Prepare(fmt.Sprintf(`
		SELECT m.verdict, m.date FROM message m
			LEFT JOIN message_recipient mr ON mr.message = m.id
			LEFT JOIN recipient r ON mr.recipient = r.id
			LEFT JOIN session s ON s.id = m.session
		WHERE (m.sender_local = ? AND m.sender_domain = ?)
			AND (r.local = ? AND r.domain = ?)
			AND s.ip = ?
			AND s.cluegetter_instance = %d
			AND m.date > FROM_UNIXTIME(UNIX_TIMESTAMP() - %d - 86400)
			AND m.verdict IS NOT NULL
		ORDER BY m.date ASC
	`, instance, tzOffset)) // sender_local, sender_domain, rcpt_local, rcpt_domain, ip
	if err != nil {
		Log.Fatal(err)
	}

	greylistGetRecentVerdictsStmt = stmt
}

func greylistGetResult(msg Message) *MessageCheckResult {
	greylistOncePrepStmt.Do(greylistPrepStmt)

	verdicts := greylistGetRecentVerdicts(msg)
	allowCount := 0
	disallowCount := 0
	for _, verdict := range *verdicts {
		if verdict.verdict == "permit" {
			allowCount = allowCount + 1
		} else {
			disallowCount = disallowCount + 1
		}
	}

	timeDiff := -1.0
	if allowCount > 0 || disallowCount > 0 {
		firstVerdict := (*verdicts)[0]
		timeDiff = time.Since((*firstVerdict.date)).Minutes()
	}
	determinants := map[string]interface{}{
		"verdicts_allow":    allowCount,
		"verdicts_disallow": disallowCount,
		"time_diff":         timeDiff,
	}

	Log.Debug("%d Got %d allow verdicts, %d disallow verdicts in greylist module. First verdict was %f minutes ago",
		(*msg.getSession()).getId(), allowCount, disallowCount, timeDiff)

	if allowCount > 0 || timeDiff > float64(Config.Greylisting.Initial_Period) {
		return &MessageCheckResult{
			module:          "greylisting",
			suggestedAction: messagePermit,
			message:         "",
			score:           1,
			determinants:    determinants,
		}
	}

	return &MessageCheckResult{
		module:          "greylisting",
		suggestedAction: messageTempFail,
		message:         fmt.Sprintf("Greylisting in effect, please come back later"),
		score:           Config.Greylisting.Initial_Score,
		determinants:    determinants,
	}
}

func greylistGetRecentVerdicts(msg Message) *[]greylistVerdict {
	StatsCounters["RdbmsQueries"].increase(1)
	verdictRows, err := greylistGetRecentVerdictsStmt.Query(
		strings.Split(msg.getFrom(), "@")[0],
		strings.Split(msg.getFrom(), "@")[1],
		strings.Split(msg.getRecipients()[0], "@")[0],
		strings.Split(msg.getRecipients()[0], "@")[1],
		(*msg.getSession()).getIp(),
	)

	if err != nil {
		StatsCounters["RdbmsErrors"].increase(1)
		panic("Error occurred while retrieving past verdicts")
	}

	verdicts := make([]greylistVerdict, 0)
	for verdictRows.Next() {
		verdict := greylistVerdict{}
		verdictRows.Scan(&verdict.verdict, &verdict.date)
		verdicts = append(verdicts, verdict)
	}

	return &verdicts
}