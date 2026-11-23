// THIRD_PARTY_NOTICES.md 만들기 (저장소 루트에서 node tools/gen-notices.mjs). atalk 실행 파일에 들어가는 모듈(go list -deps)과 Go 표준 라이브러리의 라이선스 원문을 모은다.
import { execSync } from "node:child_process";
import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const cwd = process.cwd(); // 저장소 루트에서 실행: node tools/gen-notices.mjs
const go = (args) => execSync(`go ${args}`, { cwd, encoding: "utf8" }).trim();
const cache = go("env GOMODCACHE"), root = go("env GOROOT");
const esc = (p) => p.replace(/[A-Z]/g, (c) => "!" + c.toLowerCase()); // 모듈 캐시 경로 규칙
const mods = go(`list -deps -f "{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}" ./cmd/atalk`)
  .split(/\r?\n/).map((l) => l.trim()).filter((l) => l && l.includes(" ")).filter((v, i, a) => a.indexOf(v) === i).sort();

const kind = (t) => /Apache License/.test(t) && /MIT License|Permission is hereby granted/.test(t) ? "Apache-2.0 / MIT (전환 중)" : /Apache License/.test(t) ? "Apache-2.0" : /Permission is hereby granted/.test(t) ? "MIT"
  : /Redistribution and use in source and binary forms/.test(t) ? (/Neither the name/.test(t) ? "BSD-3-Clause" : "BSD-2-Clause")
  : /blessing|public domain/i.test(t) ? "Public Domain" : "기타 (원문 참조)";

let out = `# Third-Party Notices

ATalk 실행 파일(\`atalk\`)에는 아래 오픈소스 소프트웨어가 포함되어 있습니다. 각 라이선스의 고지와 원문을 그대로 옮깁니다.
(\`ts/\` 폴더의 TypeScript 구현은 배포하지 않으며, 그 의존성 @modelcontextprotocol/sdk · zod 는 MIT 라이선스입니다.)

이 파일은 \`node tools/gen-notices.mjs\` 로 만듭니다 (\`go list -deps ./cmd/atalk\` 기준). 의존성이 바뀌면 다시 만드세요.

| 구성 요소 | 버전 | 라이선스 |
|---|---|---|
`;
const sections = [];
const add = (name, ver, dir) => {
  const files = readdirSync(dir).filter((f) => /^(licen[cs]e|copying|notice)/i.test(f)).sort();
  if (!files.length) throw new Error("라이선스 파일 없음: " + name);
  const texts = files.map((f) => [f, readFileSync(join(dir, f), "utf8").replace(/\r\n/g, "\n").trim()]);
  out += `| ${name} | ${ver} | ${kind(texts[0][1])} |\n`;
  sections.push(`## ${name} ${ver}\n\n` + texts.map(([f, t]) => `### ${f}\n\n\`\`\`\n${t}\n\`\`\``).join("\n\n"));
};
add("Go 표준 라이브러리 (golang.org)", go("env GOVERSION"), root);
for (const l of mods) {
  const [path, ver] = l.split(" ");
  if (path === "atalk") continue;
  add(path, ver, join(cache, `${esc(path)}@${ver}`));
}
writeFileSync(join(cwd, "THIRD_PARTY_NOTICES.md"), out + "\n" + sections.join("\n\n") + "\n");
console.log("모듈", mods.length - 1, "개 + Go 표준 라이브러리");
