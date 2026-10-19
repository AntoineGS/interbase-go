package interbase

import "strings"

// gdsCodesByName maps lower-case status-code names, without the isc_ prefix,
// to their InterBase numbers (iberror.h), for WHEN GDSCODE <name> matching.
var gdsCodesByName = map[string]int64{
	"arith_except":                     335544321,
	"bad_db_handle":                    335544324,
	"convert_error":                    335544334,
	"deadlock":                         335544336,
	"integ_fail":                       335544342,
	"invalid_blr":                      335544343,
	"lock_conflict":                    335544345,
	"not_valid":                        335544347,
	"no_cur_rec":                       335544348,
	"no_dup":                           335544349,
	"no_record":                        335544354,
	"read_only_trans":                  335544361,
	"read_only_view":                   335544362,
	"req_sync":                         335544364,
	"stream_eof":                       335544374,
	"random":                           335544382,
	"update_conflict":                  335544451,
	"foreign_key":                      335544466,
	"lock_timeout":                     335544510,
	"except":                           335544517,
	"primary_key_ref":                  335544530,
	"primary_key_notnull":              335544531,
	"foreign_key_notfound":             335544533,
	"primary_key_exists":               335544548,
	"check_constraint":                 335544558,
	"dsql_error":                       335544569,
	"dsql_cursor_err":                  335544572,
	"sing_select_err":                  335544652,
	"unique_key_violation":             335544665,
	"sort_err":                         335544675,
	"exception_access_violation":       335544768,
	"exception_float_divide_by_zero":   335544772,
	"exception_float_overflow":         335544775,
	"exception_float_underflow":        335544777,
	"exception_integer_divide_by_zero": 335544778,
	"exception_integer_overflow":       335544779,
	"cancelled":                        335544794,
	"exec_stmt2_execute":               335544853,
	"exec_stmt2_fetch_err":             335544854,
	"exec_stmt2_single_multi":          335544855,
	"exec_stmt_disallow":               335544857,
}

// GDSCodeByName returns the status code for a name as written in
// WHEN GDSCODE, case-insensitively and with or without the isc_ prefix.
func GDSCodeByName(name string) (int64, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "isc_")
	code, ok := gdsCodesByName[name]
	return code, ok
}
