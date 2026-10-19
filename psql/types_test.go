package psql

import "testing"

func TestParseTypeText(t *testing.T) {
	tests := []struct {
		text   string
		family TypeFamily
		length int
	}{
		{"CHAR(1)", TypeChar, 1},
		{"char ( 3 )", TypeChar, 3},
		{"CHAR", TypeChar, 1},
		{"CHARACTER(5)", TypeChar, 5},
		{"VARCHAR(20) CHARACTER SET NONE", TypeVarchar, 20},
		{"varchar(20)", TypeVarchar, 20},
		{"CHAR(2) CHARACTER SET NONE", TypeChar, 2},
		{"CHARACTER VARYING(8)", TypeVarchar, 8},
		{"CHAR VARYING(8)", TypeVarchar, 8},
		{"INTEGER", TypeInteger, 0},
		{"int", TypeInteger, 0},
		{"SMALLINT", TypeSmallint, 0},
		{"NUMERIC(15,2)", TypeOther, 0},
		{"NUMERIC(4)", TypeOther, 0},
		{"DECIMAL(18, 4)", TypeOther, 0},
		{"DOUBLE PRECISION", TypeOther, 0},
		{"DATE", TypeOther, 0},
		{"TIMESTAMP", TypeOther, 0},
		{"VARCHAR(20) CHARACTER SET UTF8", TypeOther, 0},
		{"VARCHAR(20) CHARACTER SET NONE COLLATE X", TypeOther, 0},
		{"VARCHAR", TypeOther, 0},
		{"VARCHAR(0)", TypeOther, 0},
		{"CHAR(99999999999)", TypeOther, 0},
		{"INTEGER[3]", TypeOther, 0},
		{"INTEGER NOT NULL", TypeOther, 0},
		{"BLOB SUB_TYPE 1", TypeOther, 0},
		{"", TypeOther, 0},
	}
	for _, tt := range tests {
		got := ParseTypeText(tt.text)
		if got.Family != tt.family || got.Length != tt.length || got.Text != tt.text {
			t.Errorf("ParseTypeText(%q) = %+v, want family %d length %d", tt.text, got, tt.family, tt.length)
		}
	}
}
