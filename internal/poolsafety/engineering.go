package poolsafety

// ENGINEERING TRAFFIC — the population Lens actually sells to.
//
// #391 measured CONSUMER traffic and found the populations inverted: the best genuine rephrasing
// (0.8681) scored below the worst dangerous near-identical pair (0.8836), so no threshold served
// anyone safely. That conclusion is only as broad as the corpus it came from, and that corpus was
// consumer questions.
//
// ⚠ THE INVERSION MAY NOT HOLD HERE, AND IT MAY BE WORSE. Engineering traffic differs in both
// directions: rephrasings share heavy technical vocabulary (which should raise them), and the
// dangerous pairs differ by a SINGLE TOKEN carrying the entire semantic load — a version number, a
// language name, an error code, `revert` vs `reset`. Nothing about the consumer result predicts
// which effect dominates. This measures it.

// EngineeringRephrasePairs is the same engineering question asked two ways, as a developer would
// actually type it: mixed register (formal question vs symptom report), different sentence form,
// and different vocabulary for the key concept.
//
// Deliberately NOT engineered to score well. Several pairs pit a "what is X" phrasing against a
// symptom description ("my program crashes with segfault"), because that asymmetry is the common
// real case — one person asks the concept, the next pastes the failure.
func EngineeringRephrasePairs() []RephrasePair {
	return []RephrasePair{
		{"git-revert-last", "How do I revert the last commit in git?", "Undo my most recent git commit"},
		{"node-econnrefused", "What does ECONNREFUSED mean in Node?", "Node keeps throwing connection refused, what is causing it"},
		{"css-center", "How do I center a div?", "Best way to horizontally and vertically center an element in CSS"},
		{"js-let-var", "What is the difference between let and var in JavaScript?", "When should I use let instead of var"},
		{"py-module-notfound", "How do I fix 'module not found' in Python?", "Python cannot find my import, how do I resolve it"},
		{"docker-ps", "How do I list all running Docker containers?", "Command to see which docker containers are up"},
		{"go-channels", "What is a Go channel used for?", "Explain channels in golang"},
		{"port-in-use", "How do I kill a process on port 3000?", "Something is already using port 3000, how do I stop it"},
		{"pytest-basics", "How do I write a unit test in pytest?", "Getting started with pytest test functions"},
		{"c-segfault", "What causes a segmentation fault in C?", "My C program crashes with segfault, why"},
		{"py-merge-dicts", "How do I merge two dictionaries in Python?", "Combine two dicts into one"},
		{"sql-joins", "What is the difference between INNER JOIN and LEFT JOIN?", "When does a LEFT JOIN return rows an INNER JOIN would not"},
		{"git-rebase", "How do I rebase my branch onto main?", "Steps to rebase a feature branch on top of main"},
		{"react-rerender", "Why is my React component re-rendering?", "React keeps rendering my component too many times"},
		{"go-read-lines", "How do I read a file line by line in Go?", "Golang read text file one line at a time"},
		{"js-use-strict", "What does 'use strict' do?", "Purpose of strict mode in JavaScript"},
		{"bash-env", "How do I set an environment variable in bash?", "Export a shell variable so my program can read it"},
		{"memory-leak", "What is a memory leak?", "Why does my program's memory usage keep growing"},
		{"py-parse-json", "How do I parse JSON in Python?", "Convert a JSON string to a dict"},
		{"pg-add-column", "How do I add a column to an existing table in Postgres?", "ALTER TABLE to add a new field"},
		{"js-triple-equals", "What is the difference between == and === in JavaScript?", "When should I use triple equals"},
		{"go-errors", "How do I handle errors in Go?", "Idiomatic error handling in golang"},
		{"npm-pin-version", "How do I install a specific version of an npm package?", "npm install pinned to an exact version"},
		{"race-condition", "What is a race condition?", "Two threads writing at the same time gives wrong results, what is that called"},
		{"jest-mock", "How do I mock a function in Jest?", "Replace a module's function with a stub in Jest tests"},
		{"docker-slow-build", "Why is my Docker build so slow?", "Docker image takes forever to build, how do I speed it up"},
		{"linux-large-files", "How do I find large files on Linux?", "Command to locate which files are eating disk space"},
		{"go-defer", "What does the 'defer' keyword do in Go?", "When does a deferred call actually run"},
	}
}

// EngineeringDangerPairs are questions that READ ALIKE and have DIFFERENT correct answers. Serving
// A's answer to B is a wrong answer, not a stale one.
//
// ⚠ THREE FAILURE SHAPES, deliberately, because they are not interchangeable:
//   - SAME LIBRARY, DIFFERENT VERSION — the API genuinely changed; the older answer is confidently
//     wrong and looks right.
//   - SAME ERROR, DIFFERENT CAUSE — identical symptom text, unrelated root cause and fix.
//   - SAME OPERATION, DIFFERENT LANGUAGE/SCOPE — and the destructive pairs, where the wrong answer
//     costs data rather than time (drop vs truncate, reset vs revert, stash vs discard).
//
// The single differing token carries the entire semantic load. That is precisely the case an
// embedding is worst at, and precisely the case that is most expensive to get wrong.
func EngineeringDangerPairs() []RephrasePair {
	return []RephrasePair{
		// Same library, different version — the answer changed under the same question.
		{"router-v5-v6", "How do I define routes in React Router v5?", "How do I define routes in React Router v6?"},
		{"pydantic-v1-v2", "How do I write a validator in Pydantic v1?", "How do I write a validator in Pydantic v2?"},
		{"tailwind-v3-v4", "How do I configure Tailwind CSS v3?", "How do I configure Tailwind CSS v4?"},
		{"vue-2-3", "How do I define a component in Vue 2?", "How do I define a component in Vue 3?"},
		{"django-3-5", "How do I write a migration in Django 3?", "How do I write a migration in Django 5?"},

		// Same error, different cause.
		{"econnrefused-target", "Why do I get ECONNREFUSED connecting to Postgres?", "Why do I get ECONNREFUSED connecting to Redis?"},
		{"nginx-502-504", "Why am I getting a 502 from nginx?", "Why am I getting a 504 from nginx?"},
		{"exit-137-1", "Why does my container exit with code 137?", "Why does my container exit with code 1?"},
		{"go-panic-kind", "What causes 'index out of range' in Go?", "What causes 'nil pointer dereference' in Go?"},
		{"py-import-errors", "Why does my Python script raise ImportError?", "Why does my Python script raise ModuleNotFoundError?"},

		// Same operation, different language.
		{"readfile-lang", "How do I read a file in Python?", "How do I read a file in Rust?"},
		{"http-lang", "How do I make an HTTP request in Go?", "How do I make an HTTP request in JavaScript?"},
		{"sort-lang", "How do I sort a list in Python?", "How do I sort a slice in Go?"},

		// Same shape, different SCOPE or SEMANTICS — the destructive ones.
		{"git-branch-scope", "How do I delete a git branch locally?", "How do I delete a git branch on the remote?"},
		{"git-revert-reset", "How do I revert a commit in git?", "How do I reset to a previous commit in git?"},
		{"git-stash-discard", "How do I stash my changes in git?", "How do I discard my changes in git?"},
		{"pg-drop-truncate", "How do I drop a table in Postgres?", "How do I truncate a table in Postgres?"},
		{"chmod-755-777", "What does chmod 755 do?", "What does chmod 777 do?"},
		{"kill-signal", "How do I kill a process?", "How do I kill -9 a process?"},
		{"npm-install-ci", "What does npm install do?", "What does npm ci do?"},
	}
}

// HeldOutDangerPairs is the generalisation test, and it is deliberately hostile.
//
// ⚠ WHY IT EXISTS. EngineeringDangerPairs was used to DESIGN the discriminator gate, so the gate's
// score against it is a measure of fitting, not of generalisation. Any rule set can be grown until
// it covers the corpus that produced it. These pairs were written after the gate was frozen, and
// over half name technologies deliberately absent from its lexicon (Deno, Bun, Prisma, Drizzle,
// conda, Okta, Auth0, logrotate, fetch) precisely to expose what the lexical tier cannot see.
//
// A gate that scores as well here as on the design corpus is generalising. A gate that scores
// worse has been fitted, and the gap is the honest cost of the lexical tier.
func HeldOutDangerPairs() []RephrasePair {
	return []RephrasePair{
		// Structural tier SHOULD catch these — numbers and CamelCase carry the difference.
		{"react-17-18", "How do I use useEffect in React 17?", "How do I use useEffect in React 18?"},
		{"terraform-versions", "How do I configure a listener in Terraform 0.12?", "How do I configure a listener in Terraform 1.5?"},
		{"k8s-workload-kind", "How do I write a Kubernetes Deployment?", "How do I write a Kubernetes StatefulSet?"},
		{"http-401-403", "What does HTTP 401 mean?", "What does HTTP 403 mean?"},
		{"angular-15-17", "How do I upgrade to Angular 15?", "How do I upgrade to Angular 17?"},
		{"node-18-22", "How do I write a Dockerfile for Node 18?", "How do I write a Dockerfile for Node 22?"},
		{"http-429-503", "How do I handle a 429 from the API?", "How do I handle a 503 from the API?"},
		{"pod-backoff-kind", "Why does my pod stay in CrashLoopBackOff?", "Why does my pod stay in ImagePullBackOff?"},
		{"rust-borrowck", "Why does my Rust build fail with E0382?", "Why does my Rust build fail with E0499?"},
		// Lexical tier — technologies that ARE listed.
		{"php-datastore", "How do I connect to MySQL from PHP?", "How do I connect to MongoDB from PHP?"},
		{"mock-time-runner", "How do I mock time in Jest?", "How do I mock time in Vitest?"},
		{"git-squash-amend", "How do I squash commits in git?", "How do I amend a commit in git?"},
		// ⚠ UNLISTED technologies — the lexical tier is blind here by construction.
		{"http-client", "How do I set a timeout in axios?", "How do I set a timeout in fetch?"},
		{"js-runtime", "How do I read environment variables in Deno?", "How do I read environment variables in Bun?"},
		{"orm-choice", "How do I define a model in Prisma?", "How do I define a model in Drizzle?"},
		{"signal-kind", "What does SIGTERM do?", "What does SIGKILL do?"},
		{"pkg-manager", "How do I install packages with pip?", "How do I install packages with conda?"},
		{"log-rotation", "How do I rotate logs with logrotate?", "How do I rotate logs with journald?"},
		{"sso-provider", "How do I set up SSO with Okta?", "How do I set up SSO with Auth0?"},
		// ⚠ NO ENTITY AT ALL — the difference is a plain English noun. Nothing lexical or
		// structural marks it, and no token-set rule can see it.
		{"profile-resource", "How do I profile memory usage in Python?", "How do I profile CPU usage in Python?"},
	}
}
