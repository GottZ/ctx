package store

// WriteLogEvictBatchSQLForTest is the statement of one write-log retention
// batch (issue #277): the cost pin EXPLAINs it with the arguments
// EvictWriteLog passes, and the lock tests run it inside a transaction they
// hold open, the batch in progress EvictWriteLog's autocommit statement cannot
// be paused in. Production never reads it.
const WriteLogEvictBatchSQLForTest = writeLogEvictBatchSQL
