// Copyright (c) 2023 Snowflake Computing Inc. All rights reserved.

package gosnowflake

import (
	"testing"
)

func TestUnitPostHeartbeat(t *testing.T) {
	runSnowflakeConnTest(t, func(sct *SCTest) {
		// send heartbeat call and renew expired session
		sr := &snowflakeRestful{
			FuncPost:         postTestRenew,
			FuncRenewSession: renewSessionTest,
			TokenAccessor:    getSimpleTokenAccessor(),
			RequestTimeout:   0,
		}
		heartbeat := &heartbeat{
			restful: sr,
		}
		err := heartbeat.heartbeatMain()
		if err != nil {
			t.Fatalf("failed to heartbeat and renew session. err: %v", err)
		}

		heartbeat.restful.FuncPost = postTestSuccessButInvalidJSON
		err = heartbeat.heartbeatMain()
		if err == nil {
			t.Fatal("should have failed")
		}

		heartbeat.restful.FuncPost = postTestAppForbiddenError
		err = heartbeat.heartbeatMain()
		if err == nil {
			t.Fatal("should have failed")
		}
		driverErr, ok := err.(*SnowflakeError)
		if !ok {
			t.Fatalf("should be snowflake error. err: %v", err)
		}
		if driverErr.Number != ErrFailedToHeartbeat {
			t.Fatalf("unexpected error code. expected: %v, got: %v", ErrFailedToHeartbeat, driverErr.Number)
		}
	})
}
