package poolsafety

// consumer.go — THE CONSUMER CORPUS.
//
// ⚠ THIS IS THE ONLY CONSUMER MEASUREMENT WE HAVE, and it nearly did not survive: it was built in a
// scratchpad against the live embedder and was never committed. It lives here, beside the
// engineering and held-out corpora, so the next person MEASURES rather than rebuilds — a corpus
// that has to be reconstructed is a corpus nobody re-runs.
//
// What it found, at the 0.92 threshold with the production embedder: the landlord pair scored
// 0.9770 and was ALLOWED — above threshold, so the similarity path cannot refuse it. Consumer
// questions are short, share a sentence frame, and differ in ONE entity (who, what, where, when,
// how much), which is precisely the shape cosine similarity cannot separate.
//
// The pairs are grouped by the FEATURE that differs rather than by topic, because the design
// question is not "how many pairs" but "which shapes does an exact-entity test see at all".

// COMMITTED — ConsumerUnrelatedPairs(), the ten pairs #391 measured. Kept as its own
// set so the new numbers can be compared against the ones already on record rather than replacing
// them silently.
// ConsumerCommittedDangerPairs re-exports the ten pairs #391 measured, kept as its own set so
// new numbers compare against what is already on record instead of replacing it silently.
func ConsumerCommittedDangerPairs() []RephrasePair { return ConsumerUnrelatedPairs() }

// EXTENSION — consumer danger pairs for a PUBLIC CHAT product. Every pair: reads alike, and the
// correct answer differs in a way that makes serving A's answer to B a wrong answer rather than a
// stale one. Grouped by the FEATURE that differs, because the design question is not "how many"
// but "which shapes does an exact entity test see at all".
// ConsumerDangerPairs is the consumer-product danger corpus.
func ConsumerDangerPairs() []RephrasePair {
	return []RephrasePair{
		// WHO — the person the answer is about. The single most common consumer shape and the
		// one with the worst failure mode.
		{"dose-adult-child", "How much paracetamol can I take?", "How much paracetamol can a child take?"},
		{"dose-toddler", "What is the ibuprofen dose for an adult?", "What is the ibuprofen dose for a toddler?"},
		{"safe-pregnant", "Is it safe to take ibuprofen?", "Is it safe to take ibuprofen while pregnant?"},
		{"paracetamol-cat", "Can I give my child paracetamol?", "Can I give my cat paracetamol?"},
		{"insulin-who", "How much insulin should I take?", "How much insulin should a type 1 diabetic take?"},
		{"benadryl-who", "How much antihistamine can I take?", "How much antihistamine can a baby take?"},

		// INTERACTION — what it is being combined with.
		{"alcohol-anti", "Can I drink alcohol on antibiotics?", "Can I drink alcohol on antidepressants?"},
		{"drive-after", "How long after drinking can I drive?", "How long after taking codeine can I drive?"},
		{"bleach-mix", "Can I mix bleach and vinegar?", "Can I mix bleach and washing up liquid?"},

		// SPECIES
		{"grapes-species", "Are grapes safe for dogs?", "Are grapes safe for cats?"},
		{"ibuprofen-species", "Can I give my dog ibuprofen?", "Can I give my baby ibuprofen?"},

		// JURISDICTION — same question, different legal system.
		{"drinking-age", "What is the legal drinking age?", "What is the legal drinking age in the US?"},
		{"landlord-notice-where", "How much notice must a landlord give in England?", "How much notice must a landlord give in Scotland?"},
		{"speed-limit", "What is the speed limit on a motorway?", "What is the speed limit in a residential area?"},

		// DIRECTION — same relationship, opposite party. Answer is often the mirror image.
		{"notice-direction", "How much notice must I give my landlord?", "How much notice must my landlord give me?"},
		{"refund-direction", "Can I get a refund if I change my mind?", "Can a shop refuse a refund if I change my mind?"},

		// METHOD / SUBSTANCE — one noun or verb swapped.
		{"rice-type", "How long do I cook white rice?", "How long do I cook brown rice?"},
		{"meat-temp", "What temperature should chicken be cooked to?", "What temperature should beef be cooked to?"},
		{"raw-what", "Is it safe to eat raw eggs?", "Is it safe to eat raw chicken?"},
		{"allergy-which", "What should I avoid with a peanut allergy?", "What should I avoid with a gluten allergy?"},
		{"isolate-what", "How long should I isolate with covid?", "How long should I isolate with flu?"},

		// UNITS AND FIGURES — structural in appearance, but glued to a unit.
		{"dose-mg", "Can I take 400mg of ibuprofen?", "Can I take 800mg of ibuprofen?"},
		{"oven-unit", "What is 180C in Fahrenheit?", "What is 180F in Celsius?"},
		{"mortgage-term", "Should I fix my mortgage for 2 years?", "Should I fix my mortgage for 5 years?"},

		// TIME — the entity is the year, and it is usually implicit.
		{"isa-year", "What is the ISA allowance this year?", "What was the ISA allowance last year?"},
		{"rate-when", "What is the base rate?", "What was the base rate in 2023?"},

		// SEVERITY — the second question is an emergency and the first is not.
		{"chest-pain", "What should I do about chest pain?", "What should I do about chest pain and numbness in my left arm?"},
		{"nosebleed", "How do I treat a nosebleed?", "How do I treat a nosebleed that will not stop after twenty minutes?"},
		{"fever-baby", "What should I do about a fever?", "What should I do about a fever in a newborn?"},

		// QUANTITY vs FREQUENCY — the classic dosing confusion, near-identical wording.
		{"how-much-how-often", "How often can I take paracetamol?", "How much paracetamol can I take at once?"},

		// TAX AND ELIGIBILITY — different rule, same sentence frame.
		{"tax-source", "Do I have to pay tax on savings interest?", "Do I have to pay tax on gambling winnings?"},
		{"pension-vehicle", "Can I withdraw my pension early?", "Can I withdraw from my ISA early?"},
	}
}

// EXTENSION — genuine consumer rephrasings beyond the committed 28. Same intent, different words,
// as a member of the public would type them. These measure what the gate COSTS: every one it
// refuses is a real person served a fresh generation they did not need.
// ConsumerRephrasePairs is the consumer-product rephrasing corpus — what the gate COSTS.
func ConsumerRephrasePairs() []RephrasePair {
	return []RephrasePair{
		{"passport-renew", "How do I renew my passport?", "What is the process for getting a passport renewed"},
		{"hangover", "How do I get rid of a hangover?", "Best way to feel better after drinking too much"},
		{"burn-first-aid", "What do I do for a minor burn?", "First aid for a small kitchen burn"},
		{"credit-score", "How do I improve my credit score?", "Ways to raise my credit rating"},
		{"phone-water", "My phone fell in water, what do I do?", "How to save a phone that got wet"},
		{"interview-prep", "How should I prepare for a job interview?", "Tips for doing well in an interview"},
		{"baby-sleep", "How do I get my baby to sleep through the night?", "Tips for helping an infant sleep longer at night"},
		{"plant-dying", "Why are my plant's leaves turning yellow?", "Houseplant has yellowing leaves, what is wrong"},
		{"bike-puncture", "How do I fix a puncture on a bike?", "Steps to repair a flat bicycle tyre"},
		{"quit-smoking", "How do I quit smoking?", "Best way to give up cigarettes"},
	}
}
