# Publishing a release

Commit directly to `master`. Pull requests are not required.

## First-time setup

1. Create an empty GitHub repository and configure its URL as `origin`, if needed:

   ```sh
   git remote add origin YOUR_REPOSITORY_URL
   ```

2. Review and commit the README and any outstanding changes.
3. Push the branch once before creating the first release tag:

   ```sh
   git push -u origin master
   ```

4. Open **Actions** on GitHub and confirm **Release** appears in the workflow list.
   This also makes the manual retry option available.

## Create a release

Run these commands from the project folder:

```sh
go run ./tools/release prepare v0.1.0
git push --atomic origin master v0.1.0
```

Use `v0.1.0` for the first release. For later releases, increase the version,
for example to `v0.1.1`. Preparation updates the application version when needed
and creates a local tag. The push starts the tests and builds on GitHub.

Downloads are a universal Mac DMG, a portable Windows `.exe`, Linux `.deb` and
`.tar.gz` packages, and `SHA256SUMS.txt`. Windows downloads do not include an
installer or ZIP.

## Review and publish

1. Open **Actions → Release** and wait for all builds to succeed.
2. Open **Releases** and select the draft.
3. Review the notes, test the downloads, then click **Publish release**.

The first draft includes the commit history up to its tag. Later drafts include
commit titles and descriptions since the previous published release. Edit these
notes directly in the draft if you want a shorter description.

Builds use no Apple or Windows publisher certificates. First-launch instructions
are in the [README](README.md#download-and-open).

## Retry a missing or failed draft

A draft appears only after all platform builds succeed.

- **A run failed:** open it under **Actions** and choose **Re-run failed jobs**.
- **The tag exists but no run started:** open **Actions → Release → Run workflow**.
  Leave the workflow branch set to **master**, enter the existing tag, such as
  `v0.1.0`, and click **Run workflow**.
- **A completed draft was deleted:** use the same manual retry to recreate it.

A manual retry builds the existing tag. It does not include later commits on
`master` and does not move the tag. Published releases cannot be overwritten.

## Change or cancel an unpublished release

If only the notes need changing, edit the draft directly.

For code changes, cancel any running release workflow and wait for it to stop.
Delete the draft, then fix the code and commit normally. To undo a commit, use
`git revert <commit>`.

The simplest option is to prepare a new version. You can also reuse the same
unpublished version by deleting its remote and local tags, then preparing it again:

```sh
git push origin --delete v0.1.0
git tag -d v0.1.0
go run ./tools/release prepare v0.1.0
git push --atomic origin master v0.1.0
```

Only reuse a version that has never been published. After publication, keep its
tag fixed and use a new version.

Deleting a draft or tag does not undo pushed commits. In a public repository,
pushed source is already visible; the draft controls publication of downloads.
