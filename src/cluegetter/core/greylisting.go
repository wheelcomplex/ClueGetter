// ClueGetter - Does things with mail
//
// Copyright 2016 Dolf Schimmel, Freeaqingme.
//
// This Source Code Form is subject to the terms of the two-clause BSD license.
// For its contents, please refer to the LICENSE file.
//
package core

import (
	"database/sql"
	"fmt"
	libspf2 "github.com/Freeaqingme/go-libspf2"
	"net"
	"time"
)

// A period of a month seems legit. And then we want to allow for cases
// like a news letter sent every first Monday of the month
const greylist_validity = 40

var greylistGetRecentVerdictsStmt = *new(*sql.Stmt)
var greylistSelectFromWhitelist = *new(*sql.Stmt)
var greylistUpdateWhitelistStmt = *new(*sql.Stmt)
var greylistGetWhitelist = *new(*sql.Stmt)
var greylistSpf2 = libspf2.NewClient()

type greylistVerdict struct {
	verdict string
	date    *time.Time
}

func init() {
	enable := func() bool { return Config.Greylisting.Enabled }
	init := greylistStart
	milterCheck := greylistGetResult

	ModuleRegister(&ModuleOld{
		name:        "greylisting",
		enable:      &enable,
		init:        &init,
		milterCheck: &milterCheck,
	})
}

func greylistStart() {
	greylistPrepStmt()
	greylistUpdateWhitelist()
	go func() {
		ticker := time.NewTicker(time.Duration(1) * time.Minute)
		for {
			select {
			case <-ticker.C:
				greylistUpdateWhitelist()
			}
		}
	}()

	go greylistUpdateWhitelist()
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

	stmt, err = Rdbms.Prepare(fmt.Sprintf(`
		INSERT INTO greylist_whitelist (cluegetter_instance, ip, last_seen)
		SELECT s.cluegetter_instance, s.ip, MAX(m.date)
			FROM message m
				LEFT JOIN message_recipient mr ON mr.message = m.id
				LEFT JOIN recipient r ON mr.recipient = r.id
				LEFT JOIN session s ON s.id = m.session
			 WHERE s.cluegetter_instance = %d
				AND m.date > FROM_UNIXTIME(UNIX_TIMESTAMP() - %d - 86400)
				AND m.verdict = 'permit'
			GROUP BY s.cluegetter_instance, s.ip
		ON DUPLICATE KEY UPDATE last_seen = VALUES(last_seen)
	`, instance, tzOffset))

	greylistUpdateWhitelistStmt = stmt

	stmt, err = Rdbms.Prepare(fmt.Sprintf(`
		SELECT 1 FROM greylist_whitelist
		WHERE cluegetter_instance = %d
			AND	ip = ? AND last_seen > (DATE_SUB(CURDATE(), INTERVAL 40 DAY))
		LIMIT 0,1
	`, instance))
	if err != nil {
		Log.Fatal(err)
	}

	greylistSelectFromWhitelist = stmt

	stmt, err = Rdbms.Prepare(fmt.Sprintf(`
		SELECT ip, UNIX_TIMESTAMP(last_seen) - ? ttl FROM greylist_whitelist
		WHERE cluegetter_instance = %d
			AND last_seen > (DATE_SUB(CURDATE(), INTERVAL %d DAY))
	`, instance, greylist_validity))
	if err != nil {
		Log.Fatal(err)
	}

	greylistGetWhitelist = stmt
}

func greylistUpdateWhitelist() {
	defer func() {
		if Config.ClueGetter.Exit_On_Panic {
			return
		}
		r := recover()
		if r == nil {
			return
		}
		Log.Error("Panic caught in greylistUpdateWhitelist(). Recovering. Error: %s", r)
	}()

	if Config.Redis.Enabled {
		key := fmt.Sprintf("cluegetter-%d-greylisting-schedule-greylistUpdateWhitelist", instance)
		set, err := redisClient.SetNX(key, hostname, 5*time.Minute).Result()
		if err != nil {
			Log.Error("Could not update greylist whitelist schedule: %s", err.Error())
		} else if !set {
			Log.Debug("Greylist whitelist update was run recently. Sipping")
			return
		}
	}

	t0 := time.Now()
	res, err := greylistUpdateWhitelistStmt.Exec()
	if err != nil {
		Log.Error("Could not update greylist whitelist: %s", err.Error())
	}

	rowCnt, err := res.RowsAffected()
	if err != nil {
		Log.Error("Error while fetching number of affected rows: ", err)
		return
	}

	Log.Info("Updated RDBMS greylist whitelist with %d to %d entries in %s",
		int(rowCnt/2), rowCnt, time.Now().Sub(t0).String())

	if Config.Redis.Enabled {
		greylistPopulateRedis()
	}
}

func greylistGetResult(msg *Message, done chan bool) *MessageCheckResult {
	if !Config.Greylisting.Enabled || !msg.session.config.Greylisting.Enabled {
		return nil
	}

	ip := (*msg.session).getIp()

	whitelist := msg.session.config.Greylisting.Whitelist_Spf
	res, spfDomain, spfWhitelistErr := greylistIsSpfWhitelisted(net.ParseIP(ip), done, whitelist)
	if res {
		Log.Debug("Found %s in %s SPF record", ip, spfDomain)
		return &MessageCheckResult{
			Module:          "greylisting",
			SuggestedAction: MessagePermit,
			Message:         "",
			Score:           1,
			Determinants: map[string]interface{}{
				"Found in SPF whitelist": "true",
				"SpfError":               spfWhitelistErr,
				"SpfDomain":              spfDomain,
			},
		}
	}

	if greylistIsWhitelisted(&ip) {
		Log.Debug("Found %s in greylist whitelist", ip)
		return &MessageCheckResult{
			Module:          "greylisting",
			SuggestedAction: MessagePermit,
			Message:         "",
			Score:           1,
			Determinants: map[string]interface{}{
				"Found in whitelist":     "true",
				"Found in SPF whitelist": "false",
				"SpfError":               spfWhitelistErr,
				"SpfDomain":              spfDomain,
			},
		}
	}

	if Config.Redis.Enabled {
		return greylistGetVerdictRedis(msg, spfWhitelistErr, spfDomain)
	}

	return greylistGetVerdictRdbms(msg, spfWhitelistErr, spfDomain)
}

func greylistGetVerdictRedis(msg *Message, spfWhitelistErr error, spfDomain string) *MessageCheckResult {
	determinants := map[string]interface{}{
		"Found in whitelist":     "false",
		"Found in SPF whitelist": "false",
		"SpfError":               spfWhitelistErr,
		"SpfDomain":              spfDomain,
		"Store":                  "redis",
	}

	sess := msg.session
	key := fmt.Sprintf("cluegetter-%d-greylisting-msg-%s_%s_%s", instance, sess.Ip, msg.From, msg.Rcpt)
	res, err := redisClient.Get(key).Int64()
	if err == nil {
		determinants["time_diff"] = time.Now().Unix() - res
		if (res + (int64(sess.config.Greylisting.Initial_Period) * 60)) < time.Now().Unix() {
			return &MessageCheckResult{
				Module:          "greylisting",
				SuggestedAction: MessagePermit,
				Message:         "",
				Score:           1,
				Determinants:    determinants,
			}
		}
	} else {
		redisClient.Set(key, time.Now().Unix(), time.Duration(90)*time.Minute)
	}

	return &MessageCheckResult{
		Module:          "greylisting",
		SuggestedAction: MessageTempFail,
		Message:         "Greylisting in effect, please come back later",
		Score:           sess.config.Greylisting.Initial_Score,
		Determinants:    determinants,
	}
}

func greylistGetVerdictRdbms(msg *Message, spfWhitelistErr error, spfDomain string) *MessageCheckResult {
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
		"verdicts_allow":         allowCount,
		"verdicts_disallow":      disallowCount,
		"time_diff":              timeDiff,
		"Found in whitelist":     "false",
		"Found in SPF whitelist": "false",
		"SpfError":               spfWhitelistErr,
		"SpfDomain":              spfDomain,
	}

	Log.Debug("%s Got %d allow verdicts, %d disallow verdicts in greylist module. First verdict was %.2f minutes ago",
		(*msg.session).milterGetDisplayId(), allowCount, disallowCount, timeDiff)

	if allowCount > 0 || timeDiff > float64(msg.session.config.Greylisting.Initial_Period) {
		return &MessageCheckResult{
			Module:          "greylisting",
			SuggestedAction: MessagePermit,
			Message:         "",
			Score:           1,
			Determinants:    determinants,
		}
	}

	return &MessageCheckResult{
		Module:          "greylisting",
		SuggestedAction: MessageTempFail,
		Message:         "Greylisting in effect, please come back later",
		Score:           msg.session.config.Greylisting.Initial_Score,
		Determinants:    determinants,
	}
}

func greylistIsWhitelisted(ip *string) bool {
	if Config.Redis.Enabled {
		return greylistIsWhitelistedRedis(ip)
	}
	return greylistIsWhitelistedRdbms(ip)
}

func greylistIsWhitelistedRedis(ip *string) bool {
	key := fmt.Sprintf("cluegetter-%d-greylisting-ip-%s", instance, *ip)
	return redisClient.Exists(key).Val()
}

func greylistIsWhitelistedRdbms(ip *string) bool {
	StatsCounters["RdbmsQueries"].increase(1)
	whitelistRows, err := greylistSelectFromWhitelist.Query(ip)

	if err != nil {
		StatsCounters["RdbmsErrors"].increase(1)
		Log.Error("Error occurred while retrieving from whitelist: %s", err.Error())
	} else {
		defer whitelistRows.Close()
		for whitelistRows.Next() {
			return true
		}
	}

	return false
}

func greylistIsSpfWhitelisted(ip net.IP, done chan bool, whitelist []string) (bool, string, error) {
	var error error
	for _, whitelistDomain := range whitelist {
		res, err := greylistSpf2.Query(whitelistDomain, ip)
		if err != nil {
			error = err
			Log.Error("Error while retrieving SPF for %s from %s: %s", ip, whitelistDomain, err)
			continue
		}

		Log.Debug("Got SPF result for %s from %s: %s", ip, whitelistDomain, res)
		if res == libspf2.SPFResultPASS {
			return true, whitelistDomain, error
		}
	}

	return false, "", error
}

func greylistGetRecentVerdicts(msg *Message) *[]greylistVerdict {
	StatsCounters["RdbmsQueries"].increase(1)
	verdictRows, err := greylistGetRecentVerdictsStmt.Query(
		msg.From.Local(),
		msg.From.Domain(),
		msg.Rcpt[0].Local(),
		msg.Rcpt[0].Domain(),
		(*msg.session).getIp(),
	)

	if err != nil {
		StatsCounters["RdbmsErrors"].increase(1)
		panic("Error occurred while retrieving past verdicts")
	}

	defer verdictRows.Close()
	verdicts := make([]greylistVerdict, 0)
	for verdictRows.Next() {
		verdict := greylistVerdict{}
		verdictRows.Scan(&verdict.verdict, &verdict.date)
		verdicts = append(verdicts, verdict)
	}

	return &verdicts
}

func greylistPopulateRedis() {
	defer func() {
		if Config.ClueGetter.Exit_On_Panic {
			return
		}
		r := recover()
		if r == nil {
			return
		}
		Log.Error("Panic caught in greylistPopulateRedis(). Recovering. Error: %s", r)
	}()

	Log.Info("Importing greylist whitelist into Redis")

	t0 := time.Now()
	startDate := time.Now().Unix() - (greylist_validity * 86400)

	whitelist, err := greylistGetWhitelist.Query(startDate)
	if err != nil {
		StatsCounters["RdbmsErrors"].increase(1)
		panic("Error occurred while retrieving whitelist")
	}

	i := 0
	defer whitelist.Close()
	for whitelist.Next() {
		var ip string
		var ttl uint64
		whitelist.Scan(&ip, &ttl)

		key := fmt.Sprintf("cluegetter-%d-greylisting-ip-%s", instance, ip)
		redisClient.Set(key, "", time.Duration(ttl)*time.Second)
		i++
	}

	Log.Info("Imported %d greylist whitelist items into Redis in %.2f seconds", i, time.Now().Sub(t0).Seconds())
}