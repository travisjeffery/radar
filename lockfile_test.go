package radar

import (
	"fmt"
	"strings"
	"testing"
)

// From a real dependency bump (PyJWT 2.13.0 -> 2.15.1).
const poetryBumpPatch = `@@ -2557,14 +2557,14 @@ windows-terminal = ["colorama (>=0.4.6)"]

 [[package]]
 name = "pyjwt"
-version = "2.13.0"
+version = "2.15.1"
 description = "JSON Web Token implementation in Python"
 optional = false
 python-versions = ">=3.9"
 groups = ["main"]
 files = [
-    {file = "pyjwt-2.13.0-py3-none-any.whl", hash = "sha256:66adcc2a"},
-    {file = "pyjwt-2.13.0.tar.gz", hash = "sha256:41571c89"},
+    {file = "pyjwt-2.15.1-py3-none-any.whl", hash = "sha256:42d59d63"},
+    {file = "pyjwt-2.15.1.tar.gz", hash = "sha256:4f259e80"},
 ]

 [package.extras]
@@ -3564,4 +3564,4 @@ cffi = ["cffi (>=1.17,<2.0)"]
 [metadata]
 lock-version = "2.1"
 python-versions = "^3.11, <3.15"
-content-hash = "0f1e2d3c"
+content-hash = "4b5a6978"`

// From a real dependency bump (jackson 2.22.1 -> 2.22.3).
const gradleBumpPatch = `@@ -2,9 +2,9 @@
 # Manual edits can break the build and are not advised.
 com.fasterxml.jackson.core:jackson-annotations:2.22=compileClasspath,runtimeClasspath
-com.fasterxml.jackson.core:jackson-core:2.22.1=compileClasspath,runtimeClasspath
-com.fasterxml.jackson.core:jackson-databind:2.22.1=compileClasspath,runtimeClasspath
+com.fasterxml.jackson.core:jackson-core:2.22.3=compileClasspath,runtimeClasspath
+com.fasterxml.jackson.core:jackson-databind:2.22.3=compileClasspath,runtimeClasspath
 com.github.docker-java:docker-java-api:3.7.0=testCompileClasspath`

func changesOf(s LockfileSummary) string {
	var out []string
	for _, c := range s.Changes {
		out = append(out, fmt.Sprintf("%s %s %s->%s", c.Kind, c.Package, c.From, c.To))
	}
	return strings.Join(out, "; ")
}

func TestSummarizeLockfiles(t *testing.T) {
	tests := []struct {
		name, path, patch, want string
		unattributed            int
	}{
		{name: "poetry bump", path: "rule-recommender/poetry.lock", patch: poetryBumpPatch,
			want: "upgrade pyjwt 2.13.0->2.15.1"},
		{name: "gradle bump", path: "abac-api/gradle.lockfile", patch: gradleBumpPatch,
			want: "upgrade com.fasterxml.jackson.core:jackson-core 2.22.1->2.22.3; upgrade com.fasterxml.jackson.core:jackson-databind 2.22.1->2.22.3"},
		{name: "gradle downgrade", path: "gradle.lockfile",
			patch: "@@ -1 +1 @@\n-org.yaml:snakeyaml:2.4=runtimeClasspath\n+org.yaml:snakeyaml:1.33=runtimeClasspath",
			want:  "downgrade org.yaml:snakeyaml 2.4->1.33"},
		{name: "go.sum bump", path: "infra/go.sum",
			patch: "@@ -1,2 +1,2 @@\n-golang.org/x/net v0.30.0 h1:a=\n-golang.org/x/net v0.30.0/go.mod h1:b=\n+golang.org/x/net v0.31.0 h1:c=\n+golang.org/x/net v0.31.0/go.mod h1:d=",
			want:  "upgrade golang.org/x/net v0.30.0->v0.31.0"},
		{name: "go.sum hash-only", path: "go.sum",
			patch: "@@ -1 +1 @@\n-golang.org/x/net v0.30.0 h1:a=\n+golang.org/x/net v0.30.0 h1:evil=",
			want:  "hash-only golang.org/x/net ->"},
		{name: "poetry new source", path: "poetry.lock",
			patch: "@@ -1,6 +1,11 @@\n [[package]]\n name = \"requests\"\n version = \"2.32.3\"\n files = [\n ]\n+\n+[package.source]\n+type = \"legacy\"\n+url = \"https://pypi.example.net/simple\"\n+reference = \"mirror\"",
			want:  `source-change requests ->reference = "mirror"; type = "legacy"; url = "https://pypi.example.net/simple"`},
		{name: "poetry added package", path: "poetry.lock",
			patch: "@@ -10,0 +10,4 @@\n+[[package]]\n+name = \"reqeusts\"\n+version = \"1.0.0\"\n+files = []",
			want:  "added reqeusts ->1.0.0"},
		{name: "uv hash-only", path: "uv.lock",
			patch: "@@ -1,5 +1,5 @@\n [[package]]\n name = \"idna\"\n version = \"3.10\"\n source = { registry = \"https://pypi.org/simple\" }\n-sdist = { url = \"https://files/idna-3.10.tar.gz\", hash = \"sha256:aaa\" }\n+sdist = { url = \"https://files/idna-3.10.tar.gz\", hash = \"sha256:bbb\" }",
			want:  "hash-only idna ->"},
		{name: "npm bump same registry", path: "ui/package-lock.json",
			patch: "@@ -10,6 +10,6 @@\n     \"node_modules/lodash\": {\n-      \"version\": \"4.17.20\",\n-      \"resolved\": \"https://registry.npmjs.org/lodash/-/lodash-4.17.20.tgz\",\n-      \"integrity\": \"sha512-old\",\n+      \"version\": \"4.17.21\",\n+      \"resolved\": \"https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz\",\n+      \"integrity\": \"sha512-new\",",
			want:  "upgrade lodash 4.17.20->4.17.21"},
		{name: "npm registry change", path: "package-lock.json",
			patch: "@@ -10,4 +10,4 @@\n     \"node_modules/@scope/pkg\": {\n       \"version\": \"1.0.0\",\n-      \"resolved\": \"https://registry.npmjs.org/@scope/pkg/-/pkg-1.0.0.tgz\",\n+      \"resolved\": \"https://npm.attacker.example/@scope/pkg/-/pkg-1.0.0.tgz\",",
			want:  "source-change @scope/pkg https://registry.npmjs.org->https://npm.attacker.example"},
		// An extras key named "type" is not a source.
		{name: "poetry package removed with extras", path: "rule-recommender/poetry.lock",
			patch: "@@ -1,9 +1,0 @@\n-[[package]]\n-name = \"setuptools\"\n-version = \"80.10.2\"\n-files = [\n-]\n-\n-[package.extras]\n-type = [\"mypy (==1.14.*)\"]\n-\n [[package]]\n name = \"shellingham\"",
			want:  "removed setuptools 80.10.2->"},
		{name: "poetry extras key named type changes", path: "poetry.lock",
			patch: "@@ -1,6 +1,6 @@\n [[package]]\n name = \"setuptools\"\n-version = \"80.10.2\"\n+version = \"80.11.0\"\n [package.extras]\n-type = [\"mypy (==1.14.*)\"]\n+type = [\"mypy (==1.15.*)\"]",
			want:  "upgrade setuptools 80.10.2->80.11.0"},
		{name: "poetry package removed with its source", path: "poetry.lock",
			patch: "@@ -1,6 +1,0 @@\n-[[package]]\n-name = \"internal\"\n-version = \"1.0\"\n-[package.source]\n-type = \"legacy\"\n-url = \"https://mirror/simple\"",
			want:  "removed internal 1.0->"},
		{name: "uv package added on a known registry", path: "uv.lock",
			patch: "@@ -1,4 +1,8 @@\n [[package]]\n name = \"idna\"\n version = \"3.10\"\n source = { registry = \"https://pypi.org/simple\" }\n+\n+[[package]]\n+name = \"iniconfig\"\n+version = \"2.0\"\n+source = { registry = \"https://pypi.org/simple\" }",
			want:  "added iniconfig ->2.0"},
		{name: "uv package added on a new registry", path: "uv.lock",
			patch: "@@ -1,4 +1,8 @@\n [[package]]\n name = \"idna\"\n version = \"3.10\"\n source = { registry = \"https://pypi.org/simple\" }\n+\n+[[package]]\n+name = \"iniconfig\"\n+version = \"2.0\"\n+source = { registry = \"https://evil.example/simple\" }",
			want:  `added iniconfig ->2.0; source-change iniconfig ->source = { registry = "https://evil.example/simple" }`},
		{name: "npm package added on a known registry", path: "package-lock.json",
			patch: "@@ -1,4 +1,8 @@\n     \"node_modules/a\": {\n       \"version\": \"1.0.0\",\n       \"resolved\": \"https://registry.npmjs.org/a/-/a-1.0.0.tgz\",\n     },\n+    \"node_modules/b\": {\n+      \"version\": \"2.0.0\",\n+      \"resolved\": \"https://registry.npmjs.org/b/-/b-2.0.0.tgz\",\n+    },",
			want:  "added b ->2.0.0"},
		{name: "hunk without the package name", path: "poetry.lock",
			patch:        "@@ -100,3 +100,3 @@\n files = [\n-    {file = \"x.whl\", hash = \"sha256:aaa\"},\n+    {file = \"x.whl\", hash = \"sha256:bbb\"},",
			unattributed: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ok := lockfileKindOf(tt.path)
			if !ok {
				t.Fatalf("%s not recognised", tt.path)
			}
			got := summarizeLockfile(tt.path, kind, tt.patch)
			if changesOf(got) != tt.want || got.Unattributed != tt.unattributed {
				t.Fatalf("got %q unattributed=%d, want %q unattributed=%d", changesOf(got), got.Unattributed, tt.want, tt.unattributed)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"2.15.1", "2.13.0", 1}, {"1.33", "2.4", -1}, {"v0.31.0", "v0.30.0", 1},
		{"1.0", "1.0.1", -1}, {"1.0-rc1", "1.0", -1}, {"2.22", "2.22", 0}, {"10.0", "9.9", 1},
	} {
		if got := compareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func lockfilePolicy() PullRequestPolicy {
	p := generatedPolicy()
	p.Lockfiles = &PullRequestLockfiles{RequireHumanWithoutManifest: true, RequireHumanOnSourceChange: true}
	return p
}

func lockfileReviewer(t *testing.T, p PullRequestPolicy, agent ReviewAgent) *PullRequestReviewer {
	t.Helper()
	reviewer, err := NewPullRequestReviewer(p, fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.UseGitattributes([]byte("poetry.lock linguist-generated\ngradle.lockfile linguist-generated\n")); err != nil {
		t.Fatal(err)
	}
	return reviewer
}

func file(path, status, patch string) PullRequestFile {
	return PullRequestFile{Path: path, Status: status, Additions: 1, Deletions: 1, Patch: patch, ContentComplete: true}
}

func TestLockfilesAreReviewedNotWithheld(t *testing.T) {
	agent := &recordingAgent{result: structuralVerdict()}
	in := safePullRequestInput()
	in.Files = []PullRequestFile{
		file("rule-recommender/pyproject.toml", "modified", "@@ -1 +1 @@\n-pyjwt = \"2.13.0\"\n+pyjwt = \"2.15.1\""),
		file("rule-recommender/poetry.lock", "modified", poetryBumpPatch),
	}
	got := lockfileReviewer(t, lockfilePolicy(), agent).Review(in)
	if got.Action != PullRequestWouldApprove || len(got.Withheld) != 0 || len(got.Lockfiles) != 1 {
		t.Fatalf("got %s withheld=%v lockfiles=%v; stages %+v", got.Action, got.Withheld, got.Lockfiles, got.Stages)
	}
	prompt := renderDiffForReview(agent.seen[0])
	for _, want := range []string{"upgrade pyjwt 2.13.0 -> 2.15.1", "+version = \"2.15.1\"", "rule-recommender/poetry.lock"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}

	withheld := generatedPolicy()
	if got := lockfileReviewer(t, withheld, &recordingAgent{result: structuralVerdict()}).Review(in); len(got.Withheld) != 1 {
		t.Fatalf("without lockfile review the lockfile stays withheld: %+v", got.Withheld)
	}
}

func TestLockfileBackstops(t *testing.T) {
	sourceChange := "@@ -1,4 +1,4 @@\n [[package]]\n name = \"requests\"\n [package.source]\n-url = \"https://pypi.org/simple\"\n+url = \"https://pypi.example.net/simple\""
	tests := []struct {
		name  string
		files []PullRequestFile
		want  PullRequestAction
	}{
		{"lockfile without manifest", []PullRequestFile{
			file("src/app.py", "modified", "@@ -1 +1 @@\n-a\n+b"),
			file("rule-recommender/poetry.lock", "modified", poetryBumpPatch),
		}, PullRequestRouteToHuman},
		{"manifest in another directory does not count", []PullRequestFile{
			file("other/pyproject.toml", "modified", "@@ -1 +1 @@\n-a\n+b"),
			file("rule-recommender/poetry.lock", "modified", poetryBumpPatch),
		}, PullRequestRouteToHuman},
		{"source change with manifest", []PullRequestFile{
			file("pyproject.toml", "modified", "@@ -1 +1 @@\n-a\n+b"),
			file("poetry.lock", "modified", sourceChange),
		}, PullRequestRouteToHuman},
		{"gradle lockfile with a version catalog change", []PullRequestFile{
			file("gradle/libs.versions.toml", "modified", "@@ -1 +1 @@\n-jackson = \"2.22.1\"\n+jackson = \"2.22.3\""),
			file("abac-api/gradle.lockfile", "modified", gradleBumpPatch),
		}, PullRequestWouldApprove},
		{"gradle lockfile alone", []PullRequestFile{
			file("src/Main.java", "modified", "@@ -1 +1 @@\n-a\n+b"),
			file("abac-api/gradle.lockfile", "modified", gradleBumpPatch),
		}, PullRequestRouteToHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := safePullRequestInput()
			in.Files = tt.files
			got := lockfileReviewer(t, lockfilePolicy(), &recordingAgent{result: structuralVerdict()}).Review(in)
			if got.Action != tt.want {
				t.Fatalf("got %s, want %s; stages %+v", got.Action, tt.want, got.Stages)
			}
		})
	}
}

func TestIdenticalLockfileChangesShownOnce(t *testing.T) {
	agent := &recordingAgent{result: structuralVerdict()}
	in := safePullRequestInput()
	in.Files = []PullRequestFile{file("gradle/libs.versions.toml", "modified", "@@ -1 +1 @@\n-a\n+b")}
	for i := range 50 {
		in.Files = append(in.Files, file(fmt.Sprintf("m%d/gradle.lockfile", i), "modified", gradleBumpPatch))
	}
	in.Files = append(in.Files, file("odd/gradle.lockfile", "modified", "@@ -1 +1 @@\n-org.yaml:snakeyaml:2.4=x\n+org.yaml:snakeyaml:1.33=x"))
	got := lockfileReviewer(t, lockfilePolicy(), agent).Review(in)
	if got.Action != PullRequestWouldApprove {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	prompt := renderDiffForReview(agent.seen[0])
	if n := strings.Count(prompt, "+com.fasterxml.jackson.core:jackson-core:2.22.3"); n != 1 {
		t.Fatalf("identical lockfile lines shown %d times, want once", n)
	}
	if !strings.Contains(prompt, "50 gradle lockfiles with the same changes") || !strings.Contains(prompt, "SUPPLY-CHAIN SIGNAL downgrade org.yaml:snakeyaml 2.4 -> 1.33") {
		t.Fatalf("summary not grouped or signal missing:\n%s", prompt[:min(len(prompt), 3000)])
	}
	if !strings.Contains(prompt, "+org.yaml:snakeyaml:1.33") {
		t.Fatal("a lockfile whose changes differ must keep its raw lines")
	}
}

func TestLargeLockfileIsSplitNotFailed(t *testing.T) {
	policy := lockfilePolicy()
	policy.ReviewChunkChars = 2000
	agent := &recordingAgent{result: structuralVerdict()}
	var patch strings.Builder
	patch.WriteString("@@ -1,200 +1,200 @@\n")
	for i := range 200 {
		fmt.Fprintf(&patch, "-com.example:lib%d:1.0=runtimeClasspath\n+com.example:lib%d:1.1=runtimeClasspath\n", i, i)
	}
	in := safePullRequestInput()
	in.Files = []PullRequestFile{
		file("build.gradle.kts", "modified", "@@ -1 +1 @@\n-a\n+b"),
		file("gradle.lockfile", "modified", strings.TrimSuffix(patch.String(), "\n")),
	}
	got := lockfileReviewer(t, policy, agent).Review(in)
	if got.Action != PullRequestWouldApprove {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	if len(agent.seen) < 4 {
		t.Fatalf("a %d-char lockfile under a 2000-char budget was reviewed in %d parts", patch.Len(), len(agent.seen))
	}
	seenLines := 0
	for _, part := range agent.seen {
		for _, c := range part.Changes {
			seenLines += strings.Count(c.Content, "\n+com.example:")
		}
	}
	if seenLines < 199 {
		t.Fatalf("only %d of 200 added lines reached the reviewers", seenLines)
	}
}
