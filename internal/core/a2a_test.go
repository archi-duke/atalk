package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestA2ACard(t *testing.T) {
	c := newEnv(t).core()
	a, _, _ := setup(t, c)
	desc := "ATalk 데몬 개발"
	v := ok(c.UpdateCard(a, CardPatch{Description: &desc, Capabilities: &[]string{"go"}}))
	// 할 일을 아직 안 적었으면 능력 태그로 skills 를 채운다 (A2A 는 skills 필수)
	card := ToA2A(v, "http://127.0.0.1:7420/")
	if len(card.Skills) != 1 || card.Skills[0].ID != "go" || card.SupportedInterfaces[0].URL != "http://127.0.0.1:7420/a2a/"+a {
		t.Fatalf("기본 카드: %+v", card)
	}
	skills := []Skill{{Name: "빌드·배포", Description: "go build 후 데몬 교체", Tags: []string{"go", " "}}, {ID: "review", Name: "리뷰", Description: "PR 검토"}}
	v = ok(c.UpdateCard(a, CardPatch{Skills: &skills, Cautions: &[]string{"머지는 사용자 지시가 있을 때만", ""}}))
	if v.Skills[0].ID != "skill-1" || len(v.Skills[0].Tags) != 1 || v.Skills[1].Tags == nil || len(v.Cautions) != 1 {
		t.Fatalf("정리: %+v", v)
	}
	card = ToA2A(v, "http://h")
	b, _ := json.Marshal(card)
	for _, k := range []string{`"supportedInterfaces"`, `"protocolBinding":"ATALK+HTTP"`, `"defaultInputModes"`, `"cautions":["머지는 사용자 지시가 있을 때만"]`, `"tags":[]`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("%s 없음: %s", k, b)
		}
	}
	if _, err := c.UpdateCard(a, CardPatch{Skills: &[]Skill{{Name: "x"}}}); err == nil {
		t.Fatal("description 없는 일")
	}
	if _, err := c.UpdateCard(a, CardPatch{Skills: &[]Skill{{ID: "Bad ID", Name: "x", Description: "y"}}}); err == nil {
		t.Fatal("잘못된 id")
	}
}
