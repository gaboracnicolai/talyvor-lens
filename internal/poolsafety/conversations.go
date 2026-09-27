package poolsafety

import "encoding/json"

// B16.1 — THE MULTI-TURN TRAPS. The pair corpora ask one question each; a chat asks a question
// AFTER other questions, and that is where a cached answer went wrong in production: on 27 Sep
// Nicolai asked "how much is 2+2?" after "how much is 2+3?" → "2 + 3 = 5" and was served "It's
// still 5. 🙂", another chat's answer to "so how much is 2+3?".
//
// Every Danger pair must never be served; every Rephrase pair is the same request and may be.
// cmd/pairverify measures them through cache.ConversationCandidate and the pair verifier.

// Message is one turn of a chat request.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ConversationPair is a conversation whose answer is cached (Stored) and one asked later (Asked).
type ConversationPair struct {
	Name          string
	Stored, Asked []Message
}

// ChatBody is the request the browser chat sends for msgs, which is what the cache reads.
func ChatBody(msgs []Message) []byte {
	b, _ := json.Marshal(map[string]any{"model": "claude-haiku-4-5", "max_tokens": 4096, "messages": msgs})
	return b
}

func u(s string) Message { return Message{"user", s} }
func a(s string) Message { return Message{"assistant", s} }

// then is history followed by one more user question.
func then(history []Message, q string) []Message {
	return append(append([]Message{}, history...), u(q))
}

var (
	nicolai    = []Message{u("how much is 2+3?"), a("2 + 3 = 5")}
	practising = []Message{u("I'm practising mental arithmetic, quiz me back after each answer."), a("Great, ask away!")}
	germany    = []Message{u("what is the capital of Germany?"), a("Berlin.")}
	haiku      = []Message{u("write a haiku about autumn"), a("Crisp leaves drift and fall\nthe maple lets go of red\ncold wind hums goodbye")}
	myCat      = []Message{u("I have a three-year-old cat."), a("Lovely! How can I help with your cat?")}
	bread      = []Message{u("I'm baking bread for the first time."), a("Exciting! What would you like to know?")}
)

// ConversationDanger: a different correct answer. Three families — Nicolai's own two conversations,
// one small change after an IDENTICAL history (where only the entity gate and the verifier stand),
// and the same follow-up after a DIFFERENT history (where the words are identical and only the
// history differs).
var ConversationDanger = []ConversationPair{
	{"nicolai-2+2-after-so-2+3", then(nicolai, "so how much is 2+3?"), then(nicolai, "how much is 2+2?")},
	{"nicolai-so-2+3-after-2+2", then(nicolai, "how much is 2+2?"), then(nicolai, "so how much is 2+3?")},

	{"same-history-3+3-vs-3x3", then(practising, "what is 3+3?"), then(practising, "what is 3*3?")},
	{"same-history-12-4-vs-12/4", then(practising, "what is 12-4?"), then(practising, "what is 12/4?")},
	{"same-history-15-6-vs-6-15", then(practising, "what's 15 minus 6?"), then(practising, "what's 6 minus 15?")},
	{"same-history-7>9-vs-9>7", then(practising, "is 7 bigger than 9?"), then(practising, "is 9 bigger than 7?")},
	{"same-history-5sq-vs-5cube", then(practising, "what is 5 squared?"), then(practising, "what is 5 cubed?")},
	{"same-history-7x8-vs-7x9", then(practising, "what is 7 times 8?"), then(practising, "what is 7 times 9?")},
	{"same-history-france-vs-spain", then(germany, "and what about France?"), then(germany, "and what about Spain?")},
	{"same-history-shorter-vs-longer", then(haiku, "make it shorter"), then(haiku, "make it longer")},
	{"same-history-sadder-vs-happier", then(haiku, "make it sadder"), then(haiku, "make it happier")},
	{"same-history-chocolate-vs-cheese", then(myCat, "can it eat chocolate?"), then(myCat, "can it eat cheese?")},
	{"same-history-can-vs-cannot", then(myCat, "what foods can it eat?"), then(myCat, "what foods can't it eat?")},

	{"other-history-and-france",
		then(germany, "and what about France?"),
		then([]Message{u("what is the population of Germany?"), a("About 84 million.")}, "and what about France?")},
	{"other-history-and-in-french",
		then([]Message{u("how do I say hello in Spanish?"), a("Hola.")}, "and in French?"),
		then([]Message{u("how do I say goodbye in Spanish?"), a("Adiós.")}, "and in French?")},
	{"other-history-in-fahrenheit",
		then([]Message{u("what's the boiling point of water in Celsius?"), a("100 °C.")}, "and in Fahrenheit?"),
		then([]Message{u("what's the freezing point of water in Celsius?"), a("0 °C.")}, "and in Fahrenheit?")},
	{"other-history-when-was-he-born",
		then([]Message{u("who wrote Hamlet?"), a("William Shakespeare.")}, "when was he born?"),
		then([]Message{u("who wrote Don Quixote?"), a("Miguel de Cervantes.")}, "when was he born?")},
	{"other-history-and-3+3",
		then([]Message{u("what is 2+2?"), a("4")}, "what about doubling that?"),
		then([]Message{u("what is 3+3?"), a("6")}, "what about doubling that?")},
	{"other-history-make-it-shorter",
		then(haiku, "make it shorter"),
		then([]Message{u("write a limerick about a cat"), a("There once was a cat named Lou\nwho slept in a size-seven shoe\nshe'd purr and she'd knead\nand that's all she'd need\ntill the owner went looking for two")}, "make it shorter")},
	{"other-history-is-it-safe",
		then([]Message{u("my dog ate a grape"), a("Grapes can be toxic to dogs — call a vet.")}, "is it safe?"),
		then([]Message{u("my dog ate a carrot"), a("Carrots are fine for dogs.")}, "is it safe?")},
	{"single-turn-vs-follow-up",
		[]Message{u("and what about France?")},
		then(germany, "and what about France?")},
}

// ConversationRephrase: the same request in other words, after the same history — the case the
// cache exists for. A first question has the empty history.
var ConversationRephrase = []ConversationPair{
	{"single-turn-rainbow", []Message{u("what are the rainbow colours?")}, []Message{u("which colours are in a rainbow?")}},
	{"single-turn-spider-legs", []Message{u("how many legs does a spider have?")}, []Message{u("how many legs do spiders have?")}},
	{"single-turn-boil-egg", []Message{u("how long should I boil an egg?")}, []Message{u("how many minutes do I boil an egg for?")}},
	{"same-history-knead", then(bread, "how long should I knead the dough?"), then(bread, "for how long do I need to knead the dough?")},
	{"same-history-cat-sleep", then(myCat, "how many hours a day does it sleep?"), then(myCat, "how much does it sleep each day?")},
}
