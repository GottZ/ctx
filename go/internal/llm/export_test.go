package llm

import "context"

// PrincipalCtxForTest exposes the package-internal authenticated-caller ctx to
// the external llm_test package (integration tests). MW4 removed every
// principal parameter — interactive is granted ONLY through the principal
// hook that TestMain installs for this binary, so external tests needing an
// interactive lease must use this ctx rather than reconstructing fields.
func PrincipalCtxForTest() context.Context { return testPrincipalCtx() }
