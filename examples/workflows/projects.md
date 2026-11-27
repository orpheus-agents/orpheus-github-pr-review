---
id: selected-repositories
repository_ids: [10, 11, 13]
profile: review
sandbox_template: github-review
services: [github, redmine]
language: de
notes:
  success: "✅ Prüfung abgeschlossen. Bestätigte Befunde: {{ .FindingsCount }}."
  fail: "⚠️ Die automatische Prüfung konnte nicht abgeschlossen werden."
---

# Repository review instructions

Add the review rules for repositories 10, 11 and 13 here.
