---
id: selected-repositories
repositories: [team/api, team/web, other-team/api]
profile: review
sandbox_template: github-review
services: [github, redmine]
language: de
notes:
  success: "✅ Prüfung abgeschlossen. Bestätigte Befunde: {{ .FindingsCount }}."
  fail: "⚠️ Die automatische Prüfung konnte nicht abgeschlossen werden."
---

# Repository review instructions

Add the review rules for these repositories here.
