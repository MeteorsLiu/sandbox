package methodpkg

type Receiver struct{ N int }

func (r Receiver) value() int     { return r.N }
func (r *Receiver) add(n int) int { r.N += n; return r.N }

func (r Receiver) BoundValue() func() int   { return r.value }
func (r *Receiver) BoundAdd() func(int) int { return r.add }

//go:noinline
func Value(r interface{ value() int }) int { return r.value() }

//go:noinline
func Add(r interface{ add(int) int }, n int) int { return r.add(n) }
