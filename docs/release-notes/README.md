# Release Notes

A pull request with a change that users notice adds one release note file to this
directory. At release, the notes are merged into `docs/release-notes.rst` under the new
version's heading, copy-edited, and the files are deleted.

## Writing a Note

- Name the file after the change, for example `shell-idle-timeout.rst`, and write it in
  reStructuredText.
- Start it with `:orphan:`, which keeps Sphinx from warning that the file is not in a
  table of contents.
- Put each entry under one of these categories, in this order: `**Breaking Changes**`,
  `**New Features**`, `**Improvements**`, `**Bug Fixes**`, `**Upgrade Notes**`,
  `**Security Fixes**`.
- Write each entry as a `-  ` bullet (a dash and two spaces) that starts with its
  component, such as `WebUI:`, `CLI:`, `API:`, `Master:`, `Agent:`, `Shell:` or
  `Experiments:`.
- Say in one or two sentences what changed and what the user does differently. Leave out
  how it was implemented, PR numbers and plans.
- Use double backticks for code, and spell out abbreviations (Kubernetes, not k8s).
- Run `rstfmt -w 100` on the file.

## Example

```rst
:orphan:

**Improvements**

-  Shell: A shell whose configuration sets ``idle_timeout`` ends after that long without
   activity.
```
