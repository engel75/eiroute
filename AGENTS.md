# AGENTS.md

Project-specific instructions for AI agents working on eiroute.

## GitHub Workflow

**CRITICAL: NEVER push directly to main branch.**

### Branch Strategy
- All changes → Feature branches (e.g., `feature/...`, `fix/...`, `docs/...`)
- Branch names should be descriptive: `feature/model-deprecation`, `fix/semaphore-reload`
- Never push commits directly to `main`

### Pull Request Workflow
1. Create feature branch and commit changes
2. Push feature branch to origin: `git push -u origin feature/branch-name`
3. Create PR via GitHub CLI: `gh pr create --title "..." --body "..."`
4. CI/CD builds container automatically via workflow trigger on new branches
5. **User merges PR manually via GitHub UI**

### Before Pushing
- Always verify: `git status --short`
- Ensure you're on a feature branch, not main
- Run build and tests locally: `go build ./... && go test ./...`

### Example: Completing a Feature
```bash
# On feature branch
git add . && git commit -m "feat: add new feature"
git push -u origin feature/new-feature

# Create PR for CI/CD
gh pr create --title "feat: add new feature" --body "Implements X"
```

## From CLAUDE.md

All guidelines from CLAUDE.md apply and should be merged with these instructions.

**Key reminders:**
- Think before coding - state assumptions
- Simplicity first - minimum viable code
- Surgical changes - touch only what must be changed
- Goal-driven execution - verify with concrete tests