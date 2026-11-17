import assert from "node:assert/strict";
import test from "node:test";
import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";

test("mention trigger supports Portuguese and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("Selecionado @[finding#1 X]", 25), null);
  assert.deepEqual(activeMention("Ver @Vulnerabilidade depois", 20), {
    start: 4,
    end: 20,
    query: "Vulnerabilidade",
  });
  assert.equal(activeMention("@Vulnerabilidade\npróxima linha", 25), null);
});

test("categories, Portuguese aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("Vuln").categories[0].kind, "finding");
  assert.equal(mentionSearch("Vulnerabilidade").kind, "finding");
  assert.equal(mentionSearch("Vulnerabilidade SQL injection").query, "SQL injection");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("Endpoint GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("tokens roundtrip labels and removing one reference preserves its neighbors", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "Título[1]\nDescrição" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  const value = `Analisar ${first} e ${second}`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  assert.equal(selected[0].label, "Vulnerabilidade #12 · Título(1) Descrição");
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});
