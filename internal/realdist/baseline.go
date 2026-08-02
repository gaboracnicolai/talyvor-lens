package realdist

// SyntheticEngineeringRephrasings is the 28-pair distribution every prior verdict rests on,
// recorded so a real-traffic run can be compared against it without an API call.
//
// ⚠ IT IS PINNED TO THE MODEL THAT PRODUCED IT. Cosine values are a property of the embedder;
// comparing a deployment on text-embedding-3-large against numbers from -small would repeat the
// exact error that made poolcheck's 0.6534 ceiling misleading — a figure measured on one
// population quoted at another. Callers must check SyntheticEmbeddingModel first.
const SyntheticEmbeddingModel = "text-embedding-3-small"

// Measured by `lens engcheck` on 2026-08-02, ascending.
var SyntheticEngineeringRephrasings = []float64{
	0.3395, 0.3631, 0.5145, 0.5363, 0.5413, 0.5477, 0.5530, 0.5904,
	0.6261, 0.6341, 0.6354, 0.6540, 0.6700, 0.7080, 0.7082, 0.7101,
	0.7261, 0.7310, 0.7372, 0.7486, 0.7489, 0.7770, 0.7798, 0.7939,
	0.7983, 0.8009, 0.8051, 0.8488,
}
