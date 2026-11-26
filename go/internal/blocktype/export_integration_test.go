package blocktype

// BuiltinPoliciesForTest exposes the compiled-in builtin policies to the
// EXTERNAL test package (registry_integration_test.go lives in
// blocktype_test since T4: store now imports blocktype, so an internal test
// importing testdb→store would be an import cycle). Test-only shim.
func BuiltinPoliciesForTest() []Policy { return builtinPolicies() }

// ConfigSizeForTest exposes the size measure of the config cap to the
// external test package (config_size_565_integration_test.go compares it with
// what the database renders).
func ConfigSizeForTest(raw []byte) int { return configSize(raw) }
