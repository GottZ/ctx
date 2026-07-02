package blocktype

// BuiltinPoliciesForTest exposes the compiled-in builtin policies to the
// EXTERNAL test package (registry_integration_test.go lives in
// blocktype_test since T4: store now imports blocktype, so an internal test
// importing testdb→store would be an import cycle). Test-only shim.
func BuiltinPoliciesForTest() []Policy { return builtinPolicies() }
