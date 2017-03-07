package server

import (
	"time"
)

const (
	ServerVersion                 = "dev"
	MDBInitialSize                = 1048576
	TwoToTheSixtyThree            = 9223372036854775808
	SubmissionMinSubmitDelay      = 2 * time.Millisecond
	SubmissionMaxSubmitDelay      = 2 * time.Second
	VarRollDelayMin               = 50 * time.Millisecond
	VarRollDelayMax               = 500 * time.Millisecond
	VarRollTimeExpectation        = 3 * time.Millisecond
	VarRollPRequirement           = 0.9
	VarRollForceNotFirstAfter     = time.Second
	ConnectionRestartDelayRangeMS = 5000
	ConnectionRestartDelayMin     = 3 * time.Second
	MostRandomByteIndex           = 7 // will be the lsb of a big-endian client-n in the txnid.
	MigrationBatchElemCount       = 64
	PoissonSamples                = 64
	ConfigRootName                = "system:config"
	MetricsRootName               = "system:metrics"
	HttpProfilePort               = 6060
)
