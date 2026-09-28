package poolsafety

// B21.1 — REPHRASINGS THAT NAME SOMETHING OR CARRY A NUMBER, AND DANGER PAIRS WITH EQUAL ENTITIES.
//
// Every question here names an entity or a number, so it takes the ENTITY lane: a candidate needs
// exactly the same discriminators on both sides, then the pair verifier's YES. Before B21.1 that lane
// also needed the 0.98 threshold, so none of these rephrasings — Nicolai's own questions first —
// ever reached the verifier. The danger pairs are the ones the entity gate CANNOT refuse: the same
// names and numbers, a different answer. Only the verifier stands between them and a wrong serve.
//
// Deliberately NOT in Lanes(): the pooling safety floor is measured over Lanes(), and this corpus
// measures the verifier's lane, not the similarity threshold. cmd/pairverify measures it on its own.

// EntityRephrasePairs SHOULD be served: the same question, reworded, with the same names and numbers.
func EntityRephrasePairs() []RephrasePair {
	return []RephrasePair{
		// Nicolai's questions, 27 Sep, verbatim.
		{"uk-capital", "what is the capital of the UK?", "which city is the UK's capital?"},
		{"france-capital", "what is the capital of France?", "which city is the France's capital?"},
		{"three-plus-three", "what is 3+3?", "how much is 3+3?"},
		{"three-plus-four", "what is 3+4?", "how much is 3+4?"},

		{"canada-capital", "What is the capital of Canada?", "Which city is Canada's capital?"},
		{"eiffel-height", "How tall is the Eiffel Tower?", "What is the height of the Eiffel Tower?"},
		{"hamlet-author", "Who wrote Hamlet?", "Who is the author of Hamlet?"},
		{"mona-lisa-painter", "Who painted the Mona Lisa?", "Who is the painter of the Mona Lisa?"},
		{"japan-currency", "What is the currency of Japan?", "Which currency does Japan use?"},
		{"brazil-language", "What language is spoken in Brazil?", "Which language do people speak in Brazil?"},
		{"python-reverse-list", "How do I reverse a list in Python?", "What's the way to reverse a Python list?"},
		{"sqrt-144", "What is the square root of 144?", "What's the square root of 144?"},
		{"fifteen-percent", "What is 15% of 200?", "How much is 15% of 200?"},
		{"ounces-in-pounds", "How many ounces are in 2 pounds?", "2 pounds is how many ounces?"},
		{"node-ubuntu", "How do I install Node.js 20 on Ubuntu?", "What's the way to install Node.js 20 on Ubuntu?"},
		{"london-ny-flight", "How long is the flight from London to New York?", "What is the flight time from London to New York?"},
		{"moon-distance", "How far is the Moon from Earth?", "What is the distance from Earth to the Moon?"},
		{"australia-population", "What is the population of Australia?", "How many people live in Australia?"},
	}
}

// EntityDangerPairs MUST NOT be served: equal names and numbers, a different answer.
func EntityDangerPairs() []RephrasePair {
	return []RephrasePair{
		// The pairs B21.1 names.
		{"australia-capital-vs-largest", "What is the capital of Australia?", "What is the largest city in Australia?"},
		{"python-java-direction", "Is Python faster than Java?", "Is Java faster than Python?"},
		{"okta-sso-enable-disable", "How do I enable SSO for Okta?", "How do I disable SSO for Okta?"},
		{"three-plus-three-vs-four", "what is 3+3?", "what is 3+4?"},

		{"three-plus-vs-times", "what is 3+3?", "what is 3*3?"},
		{"three-plus-vs-minus", "what is 3+3?", "what is 3-3?"},
		{"percent-vs-minus", "What is 20% of 50?", "What is 50 minus 20?"},
		{"celsius-fahrenheit-direction", "How do I convert Celsius to Fahrenheit?", "How do I convert Fahrenheit to Celsius?"},
		{"london-paris-direction", "When is the last train from London to Paris?", "When is the last train from Paris to London?"},
		{"tokyo-london-time", "What time is it in Tokyo when it is noon in London?", "What time is it in London when it is noon in Tokyo?"},
		{"usd-eur-direction", "Convert 100 USD to EUR", "Convert 100 EUR to USD"},
		{"tokyo-london-size", "Is Tokyo bigger than London?", "Is London bigger than Tokyo?"},
		{"python-upgrade-downgrade", "How do I upgrade from Python 3.11 to 3.12?", "How do I downgrade from Python 3.12 to 3.11?"},
		{"australia-population-vs-area", "What is the population of Australia?", "What is the area of Australia?"},
		{"obama-born-vs-elected", "When was Barack Obama born?", "When was Barack Obama elected?"},
		{"rice-calories-vs-carbs", "How many calories are in 100g of rice?", "How many grams of carbs are in 100g of rice?"},
		{"france-capital-vs-largest", "What is the capital of France?", "What is the largest city in France?"},
		{"uk-capital-vs-population", "what is the capital of the UK?", "what is the population of the UK?"},
	}
}
