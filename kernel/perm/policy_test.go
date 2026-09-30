package perm

import "testing"

// 授权判定顺序是安全关键：sub 点名必须压过组，组之间取最高。
func TestRoleForPrecedence(t *testing.T) {
	p := DefaultPolicy()
	p.Assign["sub-downgraded"] = RoleViewer
	p.AssignGroups["admins"] = RoleAdmin
	p.AssignGroups["ops"] = RoleOperator

	cases := []struct {
		name    string
		subject string
		groups  []string
		want    Role
	}{
		{"无映射取默认 viewer", "nobody", nil, RoleViewer},
		{"命中组", "u1", []string{"admins"}, RoleAdmin},
		{"命中多个组取最高", "u2", []string{"ops", "admins"}, RoleAdmin},
		{"组顺序不影响结果", "u3", []string{"admins", "ops"}, RoleAdmin},
		{"sub 点名压过组（可显式收紧）", "sub-downgraded", []string{"admins"}, RoleViewer},
	}
	for _, tc := range cases {
		if got := p.RoleFor(tc.subject, tc.groups); got != tc.want {
			t.Errorf("%s：RoleFor(%q,%v)=%q，期望 %q", tc.name, tc.subject, tc.groups, got, tc.want)
		}
	}
}

func TestValidateAssignGroups(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("空策略应当合法：%v", err)
	}
	p.AssignGroups["admins"] = Role("superuser")
	if err := p.Validate(); err == nil {
		t.Fatal("组映射到未知角色必须被拒绝")
	}
}
