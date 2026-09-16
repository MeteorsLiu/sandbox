# Proposal: Reflectx State Export and Reconstruction

## 1. Summary

Export reflectx-created types, their method definitions, and the associations maintained by the participating reflectx contexts. Reconstruct them with guest-local reflectx and reflect objects. References to the same host type, including `reflect.Type` values and `xtype.Type` pointers captured by ixgo, must resolve to the same guest type.

This is a research and design proposal for the new state codec. It does not implement the transfer or connect `internal/state` to production `sandbox.Run`. The source inventory was checked against the current worktree, Go 1.26.6, reflectx v1.7.8, ixgo v1.1.6, and xtype v0.3.3. Source inspection establishes the storage and constructor behavior described below; a complete export/reconstruction round trip has not been implemented or tested for this proposal.

The agreed direction is semantic export and reconstruction. The tables describe the required information, not a finalized binary format or additional public APIs. Decisions that cannot yet be established from the existing contracts are identified in section 12.

## 2. User Stories / Motivation

- An ixgo closure captures an allocation instruction's `xtype.Type` values. The guest must allocate the corresponding local types instead of dereferencing host type descriptors.
- An interpreted object has a dynamic named type with methods. The guest must retain its fields, type identity, and method behavior, including calls through interfaces.
- Several captured values and caches refer to the same type or reflectx context. Reconstruction must preserve those relationships when the interpreted code continues executing.

Use the following interpreted Go definitions throughout the proposal. The guest executable need not contain a statically compiled `Counter` type:

```go
package example

type Counter struct {
    N int
}

func (c *Counter) Add(n int) {
    c.N += n
}

type Adder interface {
    Add(int)
}

var c = &Counter{N: 3}
var a Adder = c
```

At the interpreter boundary, related runtime data can look like this:

```text
reflect.Type for Counter ------------> host type descriptor T
reflect.Type for *Counter -----------> host type descriptor P
xtype.TypeOfType(T) -----------------> the same descriptor T
reflect.Value containing c ---------> value of type P, pointing to {N: 3}
a ---------------------------------> interface {dynamic type P, value c}
Counter.Add implementation ----------> callback with its own captured objects
```

After reconstruction, `a.Add(2)` must update the restored `c.N` from 3 to 5. The descriptor addresses and interface call slots may change. All references to `Counter`, `*Counter`, and `c` must remain consistent within the guest.

## 3. Current Workaround

The current `internal/reflecttype` exports standard reflect caches and reconstructs builtins, known executable types, and supported unnamed composite types. It explicitly rejects non-static named types and has no dynamic interface reconstruction branch. Its importer also rejects cyclic dynamic type construction. See [reflecttype.go](../../internal/reflecttype/reflecttype.go) and [decode.go](../../internal/reflecttype/decode.go).

The new `internal/state` already recognizes represented `reflect.Type` values, `reflect.Value`, and `reflect.MakeFunc` callbacks. That does not make reflectx's descriptor pointers ordinary transferable objects. The reported `unknown type "Type"` concerns `xtype.Type`, whose declaration is `type Type unsafe.Pointer`. It is neither a `reflect.Type` interface nor a named struct that can be recursively copied.

The older ixgo adapter reads `TypesRecord.rcache` and rebuilds interpreted code from source. Its type discovery is useful evidence, but restarting that entire interpreter reconstruction design is not part of this proposal. The production host/guest path still uses that older value-image implementation; developing the new codec does not change production behavior by itself.

## 4. Goals

- Enumerate every reflect kind and every reflectx storage category relevant to reconstruction.
- Export type and method definitions with explicit identity, without transferring process-local runtime offsets as portable addresses.
- Restore consistent references across `reflect.Type`, `reflect.Value`, `xtype.Type`, method signatures, and context caches.
- Preserve method behavior and the distinction between direct method calls and interface calls.
- Reuse state for callback environments and ordinary values, and keep type reconstruction separate from object serialization.

## 5. Out of Scope

- Modifying reflectx, ixgo, Go, gVisor, Sentry, or the shared-library bridge in this research increment.
- Moving descriptor allocation blocks, provider backing arrays, native itabs, GC metadata, or the runtime offset map verbatim.
- Replacing the existing native closure mechanism, introducing DWARF again, or reconstructing arbitrary pointers from their numeric values.
- Solving interpreter execution state, package globals, goroutines, resources, or all callback environments merely by reconstructing their types.
- Defining a new public registration API, changing `sandbox.Run`, or choosing binary tag numbers before the integration questions are settled.

## 6. Proposal

### 6.1 Design Rule and Ownership

Transfer definitions and references; let the receiving runtime construct its own descriptors and invocation machinery.

```text
Host types + context associations             Host callback environments + values
                 |                                           |
        describe and assign TypeIDs                    state object graph
                 |                                           |
                 +--------------- transfer ------------------+
                                     |
Guest type construction -> method entry installation -> object restoration
                                                              |
                                                bind restored callbacks
                                                              |
                                                      resume execution
```

| Owner | Required responsibility | Information it should not own |
|---|---|---|
| Existing type-transfer component | Type identities, dependencies, structural definitions, and resolution to guest types. | SSA programs, interpreted function execution, or object graph traversal. |
| Reflectx-specific adaptation at the codec boundary | Read reflectx metadata, collect context associations, extract method definitions, and replay reflectx construction. | Sentry scheduling or syscall policy. |
| State object codec | Preserve object aliases and cycles; encode method callbacks, their environments, and ordinary values. | Raw reflectx method offsets and provider free lists. |
| Existing ixgo-aware caller | Supply the participating interpreter/context roots and its recorded types. | A second implementation of type identity or descriptor construction. |
| Guest reflectx/runtime | Allocate descriptors, register local offsets, create method wrappers, and allocate local interface call slots. | Host slot numbers and host runtime addresses. |

This responsibility split does not require a new public module. The placement of the adapter and its minimum internal contract remain an implementation decision in section 12.

`ReflectTypeID` identifies a type in one snapshot. `ObjectID` identifies an object in state's graph. They remain separate: the existing `reflectedType` reference connects state to the reflected type table. An object being visited first must not renumber a type discovered earlier.

### 6.2 What Reflectx Actually Stores

Reflectx has no single complete type registry. Its Context caches, runtime descriptors, and global invocation tables retain different parts of the information.

#### Context Fields

Every field below exists in [`context.go`][rx-context]. These are ordinary maps without an encompassing context lock.

| Field | Host contents | What export needs | Guest treatment |
|---|---|---|---|
| `embedLookupCache` | `map[reflect.Type]reflect.Type`; original struct to struct with extracted methods. | Both sides as TypeID associations. | Rebind both sides to local types if the context will continue serving lookups. |
| `structLookupCache` | `map[string][]reflect.Type`; string buckets with identity checks inside buckets. | Actual bucket entries and their order; strings alone do not identify types. | Rebind entries to already-created types; avoid constructing duplicate named types to populate a cache. |
| `interfceLookupCache` | `map[string]reflect.Type`; unnamed interfaces created by the context. | Key-to-TypeID associations. | Rebind to reconstructed local interfaces. Preserve the source association; do not merge other contexts by string. |
| `methodIndexList` | `map[int][]int`; provider index to allocated slot numbers for methods with `FuncId == 0`. | Ownership relationship, not the numeric slot values. | Let local method registration rebuild the allocated-slot lists. |
| `fnHasImethod` | `func(reflect.Type, Method) bool`; policy deciding whether an interface entry is allocated. | Existing methods' allocation decisions; the callback itself if future method creation must preserve its policy. | Replay existing decisions during installation. Restoring the ongoing policy callback is a separate required step for continued type creation. |
| `nAllocateError` | Accumulated allocation-failure count. | Diagnostic evidence that the source may have incomplete interface entries. | Recompute local failures; do not use the host count as guest capacity or allocation state. |

`reflectx.Default` is one context pointer. `NewContext` creates independent contexts. Ixgo uses either Default or a new context depending on `SupportMultipleInterp`; scanning Default alone is insufficient. Multiple references to one transferred context must resolve to one guest context.

#### Globals and Derived Caches

| Location | Stored information | Export / restore rule |
|---|---|---|
| `globalMethodCache` | `map[int]*ifnValue`; positive `FuncId` to the concrete T and *T method records. | Use as evidence of shared method installation. Source `FuncId` numbers and the embedded `Tfn`/`Ifn` offsets are not portable. Sharing and ownership need a guest-local mapping. |
| `globalIfnCached` | Count of allocated entries retained through positive function IDs. | Recompute through local registration. |
| `globalPtfnCache` | `(function type, method index, variadic) -> textOff`; generated pointer-receiver forwarding wrappers. | Recreate wrappers from local types and final method order. Never copy the source text offset. |
| `parserMethodTypeCache` | Function signature to input/output lists and temporary argument struct types. | Derived from the reconstructed signature; rebuild on demand. |
| `inTypeSizeCache`, `outTypeSizeCache` | Temporary argument struct type to ABI-aligned size. | Recompute in the guest ABI. |
| `abi.Default` | Ordered list of registered `MethodProvider` instances and aggregate capacity. | Guest initialization supplies providers. Preserve required behavior and sharing, not source provider addresses. |
| Register-ABI provider | Mutex, `used []unsafe.Pointer`, `free []int`, atomic count; used slots point at MakeFunc storage. | Reallocate through the provider. Its used pointers are not a uniform array of `*abi.MethodInfo`. |
| Stack-ABI provider | Mutex, `used []atomic.Pointer[abi.MethodInfo]`, free list, atomic count. | Reallocate through the provider; source slot numbers are not destination slots. |
| `zeroIfn` | Entry address of a no-op function used when no real interface entry is installed. | Regenerate the local sentinel and preserve the allocation decision. |
| `DisableAllocateWarning` | Package-global diagnostic policy. | Existing guest configuration owns it; a type transfer does not rewrite a process-global setting. |
| Standard reflect caches | Pointer/composite/function/struct constructor results. | Continue semantic type discovery; these caches include helper storage types but not every reflectx-created descriptor. |
| `runtime.reflectOffs` through `reflect.addReflectOff` | Mixed pointers to names, descriptors, and function-related storage. | Guest constructors register their own offsets. This map is not a typed enumeration of reflectx types. |
| `reflectx/x/reflect.layoutCache` | Signature/receiver to GC frame type, frame pool, and ABI description. | Recompute for local calls. This is not a closure-capture layout table. |
| `reflectx/x/reflect.assignTo`, zero-value storage, ABI constants | Local runtime entrypoint, zero buffer, and ABI-specific data. | Guest initialization supplies them. |

Sources: [`methodof.go`][rx-methodof], [`method.go`][rx-method], [`abi/abi.go`][rx-abi], register/stack provider implementations, and [`x/reflect/type.go`][rx-layout]. The built-in provider has 512 slots; additional providers can be linked. There is no public provider lookup or enumeration method for reading installed entries back out.

`Context.Reset` releases its recorded slots. `ResetAll` clears global method/provider state. Neither operation is a safe way to start an import into a runtime containing other live reflectx objects. Releasing a reconstructed context while its objects or itabs remain usable would also invalidate their invocation targets.

#### Runtime Descriptors and Related Objects

| Representation | Important stored fields | Reconstruction source |
|---|---|---|
| Common `abi.Type` / `rtype` | Size, pointer prefix, hash, flags, alignment, kind, equality function, GC data, name offset, pointer-type offset. | Local constructors and completed underlying types. Equality and GC pointers are not serialized as native closures or byte arrays. |
| `UncommonType` | Package name, total/exported method counts, method-array offset. | Named-type and method-set construction. Capacity must exist before installation. |
| Concrete `abi.Method` | `Name`, `Mtyp`, `Ifn`, `Tfn`. | Semantic method name/package/signature/callback plus local registration. `Mtyp` excludes the receiver. |
| Interface `abi.Imethod` | Name and signature offsets; no implementation callback. | Interface method definitions. Implementations belong to concrete types. |
| `reflectx.Method` | Name, package, receiver mode, receiver-free signature, callback, `FuncId`. | This is the input form needed for replay, but finalized descriptors do not retain this struct intact. |
| `abi.MethodInfo` | Method value, callback, owner type, argument layout types/sizes, receiver/variadic flags. | `setMethodSet` and the provider recreate it. It is an invocation recipe, not an independent user type definition. |
| `reflectx.MethodInfo` | A separate exported struct declared in `methodof.go`. | The inspected registration path constructs `abi.MethodInfo`, not this similarly named struct. Do not use it as a presumed export API. |
| `reflectx.Value` / `reflectx/x/reflect.Value` | Descriptor pointer, data pointer, flags; method-value flags can encode a receiver and method index. | If these concrete wrapper types are encountered, recognize their represented value. They are distinct Go types from standard `reflect.Value`; current state recognition does not automatically cover them. |
| Native interface value / itab | Concrete type, interface type, method entries, object data. | Restore types and methods first, then let the guest construct the interface value. |
| `xtype.Type` | An `unsafe.Pointer` to the represented runtime type. | Resolve the represented TypeID and call `xtype.TypeOfType` on the guest type. |

For example, `newType` allocates descriptor storage through `reflect.New(reflect.StructOf(...))`. A named function descriptor additionally reserves trailing argument type slots; a method-bearing descriptor reserves trailing method records. Copying just the common `abi.Type` prefix would omit both kinds of trailing storage.

### 6.3 Complete Type-Kind Inventory

First distinguish builtin, executable-static, and dynamically created types. Static types retain the existing same-executable resolution path, including their compiled methods. Dynamic named types need their own identity and name/package metadata in addition to the underlying kind below. Reflectx's descriptors are exposed as ordinary `reflect.Type` values; there is no separate reflectx kind enumeration to migrate.

The table enumerates all 27 `reflect.Kind` constants, including `Invalid`. Constructors show the underlying type recipe; named-type and method processing is applied separately.

| Kind | Definition to export | Guest underlying-type construction | Additional requirement |
|---|---|---|---|
| `Invalid` | No real type. | None. | Nil represented types and invalid values are value-level cases, not dynamic descriptor definitions. |
| `Bool` | Kind. | Local `bool` type. | Preserve a dynamic defined name separately. |
| `Int` | Kind. | Local `int` type. | Same target ABI for value migration. |
| `Int8` | Kind. | Local `int8` type. | Named overlay if applicable. |
| `Int16` | Kind. | Local `int16` type. | Named overlay if applicable. |
| `Int32` | Kind. | Local `int32` type. | `rune` is an alias, not a distinct runtime identity. |
| `Int64` | Kind. | Local `int64` type. | Named overlay if applicable. |
| `Uint` | Kind. | Local `uint` type. | Same target ABI for value migration. |
| `Uint8` | Kind. | Local `uint8` type. | `byte` is an alias, not a distinct runtime identity. |
| `Uint16` | Kind. | Local `uint16` type. | Named overlay if applicable. |
| `Uint32` | Kind. | Local `uint32` type. | Named overlay if applicable. |
| `Uint64` | Kind. | Local `uint64` type. | Named overlay if applicable. |
| `Uintptr` | Kind. | Local `uintptr` type. | Reconstructing its type does not identify address-valued integers in objects. |
| `Float32` | Kind. | Local `float32` type. | Named overlay if applicable. |
| `Float64` | Kind. | Local `float64` type. | Named overlay if applicable. |
| `Complex64` | Kind. | Local `complex64` type. | Named overlay if applicable. |
| `Complex128` | Kind. | Local `complex128` type. | Named overlay if applicable. |
| `Array` | Length and element TypeID. | `reflect.ArrayOf(length, elem)`. | Element layout must be valid; the runtime's associated slice descriptor is locally derived. |
| `Chan` | Direction and element TypeID. | `reflect.ChanOf(direction, elem)`. | Channel contents/lifecycle belong to state's value policy, not this definition. |
| `Func` | Input/output TypeIDs in order and variadic bit. | `reflect.FuncOf(in, out, variadic)`. | No PC belongs in a function type. A function value separately needs its callback or native PC/environment. |
| `Interface` | Complete method names, package identities, and receiver-free signature TypeIDs. | `Context.InterfaceOf`; named forms use `NewInterfaceType` / `SetInterfaceType` or the named underlying-type path. | Preserve unexported method identity. No callback is stored in the interface definition. |
| `Map` | Key and element TypeIDs. | `reflect.MapOf(key, elem)`. | Guest creates hasher, group/bucket layout, flags, and GC data. Never transfer those pointers. |
| `Pointer` (`Ptr`) | Element TypeID. | `reflectx.PtrTo(elem)` where the reflectx method-bearing pointer descriptor must be retained. | `Ptr` and `Pointer` are aliases for one kind. Reuse the *T descriptor paired with T's method set. |
| `Slice` | Element TypeID. | `reflect.SliceOf(elem)`. | Length, capacity, and backing contents belong to values. |
| `String` | Kind. | Local `string` type. | Bytes belong to values. |
| `Struct` | Ordered fields: name, package, tag, anonymous bit, and TypeID. | `Context.StructOf(fields)` for reflectx structures. | Preserve blank/private/embedded fields; regenerate equality and GC metadata. Check reconstructed offsets, size, and alignment against the source layout. |
| `UnsafePointer` | Kind. | Local `unsafe.Pointer` type. | Does not authorize migration of arbitrary pointed-to memory. `xtype.Type` has its own known representation rule. |

Additional classifications apply across those kinds:

| Type category | Export and reconstruction rule |
|---|---|
| Executable-static type | Reuse the local executable type through existing type resolution. Do not turn it into a new dynamic named type. |
| Dynamic named type, including named scalar/container/function | Preserve source identity, package, name, underlying definition, and methods. Use `NamedTypeOf` and, where needed, preallocated method storage plus `SetUnderlying`. |
| Dynamic unnamed struct with promoted methods | Preserve its structural fields and complete effective method set. `StructOf` alone does not represent the whole result of `StructToMethodSet`. |
| Dynamic unnamed interface | Preserve the effective method set; original source embedding syntax is not required for runtime use. Cache identity and private method package identities still matter. |
| Named interface | Preserve the name/package as well as its effective interface method set. It has no concrete implementation callbacks. |
| Recursive or mutually recursive types | Reserve TypeIDs before following dependencies; construct stable identities before linking cyclic references. See section 6.8. |
| Type aliases | Reuse the aliased runtime type; do not invent an extra runtime identity. |
| Instantiated generic/local named types | Keep distinct host `reflect.Type` identities distinct even when displayed names match. Existing executable types remain static; dynamic definitions follow the named path. |
| Open type parameters or constraint-only interfaces | These are not ordinary instantiated runtime value types obtainable from the reflectx recipes here. Do not infer them from `go/types.Type` and claim runtime support. |

The raw descriptor structs corresponding to composite kinds are `ArrayType`, `ChanType`, `FuncType`, `InterfaceType`, `MapType`, `PtrType`, `SliceType`, and `StructType`. Basic kinds use the common prefix. `Name`, `StructField`, `Method`, `Imethod`, and `UncommonType` supply metadata rather than additional Go value kinds.

### 6.4 Discover the Complete Participating Set

"Export all" means all definitions and associations owned by the participating contexts/interpreters, plus everything reachable from the transferred graph. It cannot mean every descriptor ever allocated anywhere in the process: reflectx exposes neither a list of every context nor a complete registry of `NamedTypeOf` results.

| Discovery source | What it contributes | Why it is insufficient alone |
|---|---|---|
| Ixgo `TypesRecord.rcache` | The runtime types recorded by that interpreter, paired with `go/types.Type`. Read its actual reflect.Type keys; exporting the entire go/types graph is unnecessary for this inventory. | External types can be obtained through the loader; callers can create other types. |
| Participating reflectx Context caches | Struct, interface, and extracted-method types, including association keys and values. | A directly created `NamedTypeOf` result need not be in any of these maps. |
| State's reachable objects | `reflect.Type` values, represented `reflect.Value` types, closure environments, contexts, and known xtype pointers. | Does not include unused definitions that must remain in a transferred context. |
| Standard reflect caches | Cached pointer/container/signature/struct types and their dependencies. | A helper struct used to allocate a descriptor is not the descriptor it contains. |
| `reflectx.TypeLinks` / `TypesByString` | Executable typelink entries or matching entries from them. | These helpers read typelinks, not a registry of dynamically allocated reflectx types. |
| Type dependency traversal | Element/key/field/signature/interface types, paired pointer types, and concrete methods. | Requires correct initial roots. |
| Method callback traversal | Types and contexts captured by method implementations and policy callbacks. | Can discover more object/type dependencies; discovery must continue until both queues stop growing. |

The old adapter already reads `TypesRecord.rcache` through private-field access in [ixgo_linux.go](../../ixgo_linux.go). This establishes a non-invasive caller-side discovery path. Reading these plain maps requires quiescent type creation and context reset; Go's standard `sync.Map` cache behavior does not make reflectx's maps safe to enumerate concurrently.

Keep actual `reflect.Type` references alive throughout discovery. For every known type, derive a host-descriptor-address to TypeID mapping. An encountered nonzero `xtype.Type` must match a known type or a separately verified type-descriptor root. It must not cause an arbitrary address to be interpreted as an ABI descriptor.

The current state caller makes a type discoverable to parameterless `reflecttype.Export()` by calling `reflect.SliceOf(typ)`. The same caller-side technique can expose collected roots without changing that API merely to pass a list. It does not solve named-type rejection, method export, or the need to feed callback objects into state.

Export must finish discovering callback environments before freezing the type table. The existing sequence "finish object traversal, then Export" needs coordination when type export itself discovers new method callbacks. A complete transfer is a fixed point of type and object discovery, not one independent pass over each.

### 6.5 Logical Transfer Contents

The following names describe information, not new exported Go structs or finalized wire tags.

| Contents | Required information | Example |
|---|---|---|
| Type definitions | Snapshot identity; static location or dynamic kind/name/package/dependencies. | `R17 = example.Counter {N: R1}`. |
| Method definitions | Owner TypeID; name/package; receiver mode; signature TypeID; callback object reference; interface-entry behavior. | `R17.Add = pointer receiver, R19, callback O41`. |
| Context associations | Source-context identity; cache associations expressed with TypeIDs; policy callback reference when continued construction requires it. | Context C1 maps a struct cache entry to R17. |
| Value graph | Existing state object records, including callback environments, interface contents, and ordinary values. | `O42 = Counter{N:3}`. |
| Known type-pointer values | Represented TypeID plus the known value representation. | A captured `xtype.Type` denotes R17. |

Source `FuncId`, provider indices, and `textOff`/`typeOff`/`nameOff` numbers may help export identify relationships. They are not written back as valid guest addresses or slot identities. Exact encoding of shared method groups and installation policy is an open decision; inventing a second independent serializer is unnecessary.

### 6.6 Extract Concrete Methods

For the running example, the input needed by reflectx is equivalent to:

```text
Owner:       Counter (R17)
Name:        Add
Package:     example
Pointer:     true
Signature:   func(int) (R19), excluding the receiver
Callback:    func([]reflect.Value) []reflect.Value (object O41)
```

Export follows this sequence:

1. Classify the type as executable-static or dynamically constructed. Static methods stay with their executable type.
2. For a dynamic concrete type T, inspect both T and *T. Use `NumMethodX` for the complete non-interface method count, not only `Type.NumMethod`, which omits unexported concrete methods.
3. Match methods by name and package identity across the two sets. A method present on T uses the T implementation once; the corresponding *T forwarding wrapper is derived. Methods present only on *T require their pointer-receiver implementations.
4. Recover the receiver-free signature. The descriptor's `Mtyp` has that form; `MethodByIndex(...).Type` includes the receiver and must be adjusted if used instead.
5. For reflectx-generated concrete methods, read the original MakeFunc callback. `createMethod` makes `mfn = reflect.MakeFunc(ftyp, m.Func)` and registers its storage in `Tfn`. The existing state MakeFunc extraction can supply the callback once the method value is correctly represented. Do not serialize the `Tfn` offset as the method PC.
6. Add the callback to state's ordinary object queue. Export its captured objects using the same alias/cycle tracking as the root closure.
7. Inspect the effective interface entries on both T and *T, and identify sharing that affects slot ownership/capacity. In the inspected gc implementation, an intentional skip retains `zeroIfn`; exhausted registration returns a nil target, increments `nAllocateError`, and causes `setMethodSet` to return an allocation error. Do not merge these states or call a source policy callback during inspection merely to guess its previous decision.

There are concrete gaps in the public inspection API:

| Gap | Source evidence | Required treatment |
|---|---|---|
| Unexported concrete method package identity | `rtypeMethodX` builds Name/Type/Func but does not assign `reflect.Method.PkgPath`. | Read the encoded name's package information, with the declaring type's package where the ABI requires it. An empty public field is not proof of an exported method. |
| Interface enumeration | `NumMethodX` reads the uncommon concrete-method area. Interface signatures live in `InterfaceType.Methods`. | Use the interface method list through the interface reflection path. |
| Original method input | Final descriptors do not retain a `reflectx.Method` or its original `FuncId`. | Recover definitions and sharing from descriptors plus caches; do not assume a public ExportMethods function exists. |
| Unused interface methods | Ixgo's `fnHasImethod` can deliberately reject a method. `zeroIfn` is used in that case. | Preserve the observed installation decision. Forcing every method to allocate can exceed capacity and change behavior. |
| Missing code | Static linker-elided entries or unavailable callbacks cannot be reconstructed from signatures. | Report the exact method and missing implementation. |

These are bounded, version-specific reads inside the adapter. They do not justify exposing raw descriptor manipulation to library users or changing upstream reflectx.

### 6.7 Encode Sequence

```text
collect participating interpreters and contexts
enqueue TypesRecord keys, context type entries, and root value

while type queue or object queue is nonempty:
    object:
        use state for ordinary fields, references, and callback environments
        recognize represented types and xtype.Type before raw pointer scanning
        enqueue newly discovered types and contexts

    type:
        reserve one TypeID before following dependencies
        describe the kind/name/package and dependent TypeIDs
        extract concrete methods and enqueue their callback objects

    context:
        describe its cache associations using the same TypeIDs
        enqueue policy callback only where its ongoing behavior is required
        retain slot ownership information for guest reconstruction

after both graphs are closed:
    freeze TypeIDs and method-to-ObjectID references
    write type definitions and method/context metadata
    write the existing state object graph
```

The `xtype.Type` conversion is based on a represented type, not on the static Go type of the pointer wrapper:

```text
Host value:  xtype.Type(0x12340000)
Host index:  0x12340000 -> Counter -> R17
Transfer:   represented type R17
Guest:      types[R17] -> local Counter descriptor
Guest value: xtype.TypeOfType(types[R17])
```

The hexadecimal address is illustrative. `xtype.Type(nil)` remains nil. Both nil and non-nil values require recognition before the generic unsafe-pointer branch, including zero-value detection in state.

### 6.8 Restore Types, Methods, Then Objects

The required ordering is stricter than "create types, decode objects, add methods."

| Stage | Guest action | What is valid afterward |
|---|---|---|
| 1. Read definitions | Resolve existing static types; reserve entries for dynamic identities and contexts. | References can name types that have not yet been completed. No user code runs. |
| 2. Allocate final identities | Allocate named placeholders with the correct kind/storage capacity. Reserve method storage before publishing T or constructing its dependent *T. | Every dependent reference can point to the final descriptor identity. |
| 3. Complete layouts | Build underlying types, fill recursive references, and establish canonical local composite types. | Size, alignment, fields, equality, GC information, and function signatures are ready for value allocation. |
| 4. Install method entries | Install complete signatures and local method/Ifn entries with deferred callback slots; preserve allocation decisions. | Interface conversion and itab construction can observe the final method set. Implementations are not yet callable. |
| 5. Decode the object graph | Allocate values with the completed local types; restore aliases, interfaces, callbacks, and known xtype pointers. | Method callback slots can be bound to decoded callback objects. |
| 6. Complete associations | Fill deferred callbacks, rebind context caches, and restore the ongoing policy where required. | All referenced local state is connected. |
| 7. Publish | Run any restore hooks that may invoke methods only after the above dependencies are complete, then return the restored root. | User execution may resume. |

`NewMethodSet` returns a new T descriptor and a paired *T descriptor. Applying it after publishing a previous T would split identity: existing field/signature/cache references would retain the old T. Reserve the final method-bearing identity first.

Recursive example:

```go
type Node struct {
    N    int
    Next *Node
}
```

```text
R30 = Node {N: int, Next: R31}
R31 = *R30

allocate final Node descriptor using a layout-compatible placeholder
publish R30 internally
construct or reuse its final pointer descriptor R31
build underlying struct {N int; Next types[R31]}
SetUnderlying(types[R30], underlying)
```

Ixgo's `toMockType` followed by `NamedTypeOf`, `NewMethodSet`, and `SetUnderlying` provides a source example of this construction order. It is not safe to use `struct{}` as the placeholder for every kind. A named function such as `type F func(F) F` must reserve the original input/output counts in its descriptor before `SetUnderlying` writes the type pointers.

The importer must also respect constructors that derive metadata from dependency layouts. Array element layouts and map key equality/hash behavior cannot be built from arbitrary incomplete placeholders. Recursive maps, blank fields, mutually recursive interfaces, and unnamed cycles require targeted construction tests; the existing ixgo pattern alone does not prove every reflectx-created graph can be replayed safely.

### 6.9 Break the Method/Value Cycle

The need to install methods before decoding values comes from an actual caller: state's `decodeInterface` ends by assigning the restored concrete value to its destination interface. Go reflection checks method compatibility and can construct an itab at that assignment. Installing methods only afterward is too late.

At the same time, a method callback can capture objects whose types are still being restored. A local forwarding callback can break that cycle:

```go
// Illustrative guest-side construction, not a new public API.
var restoredAdd func([]reflect.Value) []reflect.Value

forwardAdd := func(args []reflect.Value) []reflect.Value {
    return restoredAdd(args)
}

// Install Counter.Add with forwardAdd while setting up the type.
// Decode the state graph, including callback object O41.
// Then assign restoredAdd = decoded callback O41.
// Only then publish the restored root or invoke restore hooks.
```

This also works with the provider's own generated wrappers: every path ultimately reaches the same forwarder and its bound callback. No path may invoke that forwarder before it is bound. All references to a shared callback must bind to the same decoded object; per-method installation must not duplicate its captured mutable state.

The current state decoder fills deferred MakeFunc callbacks before running completion callbacks. Reflectx binding must participate in that existing ordering. Adding an after-Load method fixup would miss interface conversion and potentially user restore hooks.

### 6.10 Example Snapshot and Result

The labels below are illustrative, with separate type, object, context, and method namespaces:

| Entry | Transferred information |
|---|---|
| R1 | Builtin `int`. |
| R17 | Named `example.Counter`, Struct, field `N: R1`, method M1. |
| R18 | Pointer to R17, paired with the method-bearing Counter descriptor. |
| R19 | Signature `func(int)`, input R1, no results, non-variadic. |
| R20 | Named `example.Adder`, interface method `Add: R19`. |
| M1 | Owner R17, `Add`, pointer receiver, signature R19, callback O41, real interface entry required. |
| O41 | State-encoded method callback and its reachable environment. |
| O42 | Counter object, `N = 3`. |
| Root.c | Pointer of type R18 to O42. |
| Root.a | Interface R20 containing dynamic type R18 and pointer to O42. |
| Root.type | Represented `reflect.Type` R17. |
| Root.allocType | Represented `xtype.Type` R17. |
| C1 | Participating context associations, all referring to these same TypeIDs. |

```text
Guest type phase:
    R17 -> new local Counter descriptor
    R18 -> its local *Counter descriptor
    M1  -> local reflectx method wrapper + local interface entry
           callback slot exists but is not yet bound

Guest value phase:
    O42 -> allocated Counter{N:3}
    c   -> &O42
    a   -> interface containing that same &O42
    type      -> types[R17]
    allocType -> xtype.TypeOfType(types[R17])
    O41 -> restored callback

Completion:
    bind M1 to O41
    finish C1 associations
    publish root

Execution:
    a.Add(2)
      -> guest itab entry
      -> guest reflectx interface-call slot
      -> local method callback forwarder
      -> restored O41 implementation
      -> O42.N = 5
    c.N == 5
```

For ixgo, O41 can still reach interpreted execution state. `Interp.FindMethod` returns a callback capturing `pfn` and `mtyp`, and `pfn.callFunctionByReflect` uses its interpreter. Special treatment of reflectx removes descriptor/provider traversal; it does not by itself eliminate those interpreter dependencies.

### 6.11 Context Identity, Concurrency, and Lifetime

| Concern | Required rule / remaining decision |
|---|---|
| Multiple contexts | Discover actual participating contexts; preserve aliases; never assume Default contains them all. |
| Same type in several contexts | One snapshot TypeID for one host type identity, even if several caches refer to it. Installation ownership needs explicit accounting. |
| Distinct types with the same printed name | Keep distinct IDs. String formatting is not identity. |
| Concurrent mutation | Discovery requires stable context maps, type descriptors, callback environments, and method tables. No local adapter mutex can freeze unrelated callers of upstream constructors or Reset. |
| Existing guest state | Do not clear providers or overwrite Default. Reuse static types; integrate dynamic ownership without invalidating other live objects. |
| Failure halfway through reconstruction | Do not publish partial values. Newly allocated context-owned slots can be released only when nothing exposed still uses them. Runtime type caches and offset registrations do not offer a general rollback/unregister operation. |
| Completion lifetime | Reconstructed method targets must remain installed for as long as any restored object, cached type, or interface can call them. Tying cleanup only to the decode call is insufficient. |
| Positive `FuncId` sharing | Upstream caches these entries globally and excludes them from per-context slot lists. Fresh guest IDs and ownership must avoid collisions and capacity inflation. Do not silently turn every method into a separately allocated `FuncId == 0` method. |
| Future method creation | Preserve the original `fnHasImethod` policy when continued creation is allowed. Replaying saved decisions covers only already-installed methods. |

## 7. Error Handling

These are semantic failure conditions to report with the object/type/method path. Exact error types and wording are not defined here.

| Condition | Required behavior |
|---|---|
| Nonzero xtype pointer has no known represented type | Stop with its capture/field path; do not copy the address or guess a pointee. |
| Missing type/context root | Report incomplete discovery; do not claim a complete snapshot. |
| A dynamic type is unsupported by the inspected construction path | Report kind, name, and the unsupported dependency. |
| Private method package identity cannot be recovered | Stop before constructing a method with different interface identity. |
| Callback or native function metadata cannot be migrated | Report the method and the failing callback dependency. |
| Guest method provider lacks required capacity | Fail installation before publishing values. The host's old slot number cannot supply capacity. |
| Source had incomplete interface allocation | Report the failed entry. A nil target from allocation failure is different from the local `zeroIfn` sentinel used for an intentional skip. |
| Reconstructed field offsets, alignment, signature, or method set differ | Fail before allocating/restoring dependent values. |
| Construction or Reset races with discovery | Require a stable capture boundary; do not present a partially observed graph as a consistent snapshot. |
| Deferred callback remains unbound | Do not publish the restored root or run method-invoking restore hooks. |

## 8. Compatibility

This proposal requires no change to the user-facing `sandbox.Run` or the Sentry design. It extends the new codec's interpretation of known runtime objects. It is not a backward-compatible promise for serialized images: binary schema changes and version handling must be decided when implementing the adapter.

| Environment / path | Evidence and implication |
|---|---|
| Go gc 1.26.6 | Existing reflecttype code explicitly requires this version. Private layout readers must match it; enabling linkname does not establish compatibility with another version. |
| Linux amd64 and arm64 | Existing native/MakeFunc codec paths target these ABIs. Both require independent method-transfer verification; source inspection is not a completed integration test. |
| Other gc architectures / ABI0 | Reflectx has separate provider representations and generated entrypoints. Inventory includes them; no claim that the existing state native-closure code supports them follows from this proposal. |
| macOS, Windows, WebAssembly | Reflectx lists them as library platforms. That is distinct from Sentry availability and the codec's ELF/native metadata path. Research coverage does not broaden production sandbox support. |
| LLGo | `rtype_llgo.go` uses different descriptors/text references, special named-function handling, and `reflect.namedFuncMap`; its IcallStat/IcallAlloc functions return zero. The gc export reader cannot be reused unchanged. |
| Go 1.24-1.26 versus 1.27 map layouts | Reflectx selects different map descriptor/clone implementations. Transfer key/element semantics and let the selected local implementation rebuild metadata. |
| Same executable and loaded modules | Preserve current static-type and native-PC assumptions. This is not cross-binary or cross-architecture checkpoint conversion. |
| Existing registered state types | Keep their current mechanism. Adding reflectx reconstruction does not require removing `Register` or merging its namespace with reflected TypeIDs. |

The document is the only repository change in this step. Implementation will touch type transfer, object decoding order, and version-specific reflection; it therefore requires validation beyond the original `unknown type "Type"` case.

## 9. Alternatives Considered

### 9.1 Copy Raw Descriptors and Runtime Offset Maps

Descriptor fields reference names, other types, GC data, equality/hash functions, MakeFunc storage, and provider entries. A byte copy neither makes those references local nor establishes slot ownership. The runtime offset map also mixes several pointer categories.

### 9.2 Serialize Reflectx Internals as Ordinary State Objects

This traverses process-local descriptor and provider machinery, including untyped addresses. It also mistakes derived call-frame pools and caches for required user state. The existing xtype failure is one direct example of the mismatch.

### 9.3 Export Only Public `reflect.Type` Methods

Public reflection gives most structural information but cannot reconstruct dynamic named types or arbitrary interfaces on its own. Reflectx's public inspection helpers also omit some finalized method information needed for an exact replay, especially private package identity and slot-sharing relationships.

### 9.4 Rebuild All of Ixgo From Source

Recompilation can recreate reflectx state, but expands the task into interpreter/package/source restoration. It is not necessary merely to describe dynamic types. Callback execution dependencies remain visible rather than being silently replaced with this older design.

### 9.5 Allocate Every Method Again Without Preserving Policy or Sharing

This is simple but can allocate entries for deliberately skipped methods, lose shared positive-ID installations, and exhaust the finite provider capacity. It cannot be advertised as equivalent reconstruction without defining that behavior change.

## 10. Testing Strategy

No implementation tests were run for this document-only change. The following matrix is the acceptance plan, with separate-process restoration necessary to catch accidental reuse of host addresses and globals.

| Test family | Required observations |
|---|---|
| Every kind in section 6.3 | Kind, size, alignment, comparability, and all structural dependencies survive; Invalid remains a non-type. |
| Dynamic defined basic/container/function types | Name/package identity survives; a named type is not replaced by its underlying builtin or composite. |
| Cache-only and reachable-only roots | Types absent from Default or the standard reflect caches are found through their real owners. |
| Static plus dynamic mixtures | Compiled types remain the same guest executable types; dynamic dependencies reuse their snapshot identities. |
| Duplicate names and multiple contexts | Equal source identities stay equal; distinct identities do not collapse by name. |
| Private, blank, repeated blank, embedded, and tagged fields | Correct fields/offsets; comparability and map-key behavior match, including ignored blank fields. |
| Recursive types | Node pointers, named recursive functions, slices/maps, nested arrays, and mutual interface references restore without stale placeholders or metadata. |
| Concrete methods | Value and pointer receivers, exported/private methods, variadic calls, zero-sized receivers, embedded/promoted methods, and overridden methods. |
| Dynamic interfaces | Direct/reflected interface assignment and subsequent calls; same-name private methods from different packages stay distinct. |
| Callback cycles and shared captures | A callback capturing an object of its own method-bearing type restores; aliases across methods and the root remain intact. |
| Method installation order | Interface values are decoded after final method entries exist; no hook invokes an unbound callback. |
| Skipped and shared slots | Preserve effective source behavior; measure real slot use; avoid double release or collision with existing guest installations. |
| Provider capacity exhaustion | Failure publishes no callable partial object; existing guest methods still work. |
| `xtype.Type` | Nil, builtin, static named, dynamic named, and closure-captured T/*T pointers all resolve through the same local type table. |
| Reflection wrappers | Standard `reflect.Value` and any encountered concrete reflectx wrapper preserve their represented type/value; method receiver/index cases are explicit. |
| Cache reuse after import | A continuing interpreter lookup returns the restored types instead of fresh duplicate descriptors. |
| Subsequent construction policy | Existing methods and later methods honor the restored `fnHasImethod` policy where this mode is supported. |
| Lifetime and repeated transfers | Re-save restored graphs, keep objects callable for their intended lifetime, and avoid invalidating other contexts during cleanup. |
| ABI and version paths | Run the supported Linux amd64 and arm64 paths independently; gate private-layout assumptions and document other unverified paths. |

## 11. Summary of Changes

| Area | Proposed work |
|---|---|
| Type transfer | Add dynamic named/interface definitions and recursive construction while preserving the existing reflected-type identity table. |
| Reflectx adaptation | Discover owners, export method definitions and context associations, and recreate local method/provider state. |
| State | Recognize xtype type-pointer values; coordinate callback/type discovery and bind method callbacks before execution or relevant restore hooks. |
| Existing ixgo caller | Supply its actual type/context roots through the already-demonstrated inspection path. |
| Sentry / production sandbox path | No change in this increment. |
| This research step | Add this proposal only; preserve existing implementation work and the earlier proposal. |

## 12. Open Questions

1. **Adapter contract and placement.** Caller-side root collection is possible, but current `reflecttype.Export/Open` cannot independently serialize state callbacks or complete methods that depend on those callbacks. Define the smallest internal coordination data before changing either module's API. Preserve parameterless Export unless a concrete requirement proves it insufficient.
2. **Private method extraction.** Verify callback extraction, private method package decoding, and current method order against actual reflectx-generated descriptors. Public `MethodByIndex` alone does not provide every required field.
3. **Shared installation ownership.** Final descriptors do not store original FuncId values. Determine how to recover positive-ID sharing, allocate collision-free guest identities, and retain/release their slots without importing upstream global counters wholesale.
4. **Interface-entry replay.** Preserve the separate T and *T installation states, including intentional skips and shared entries. The source distinguishes a `zeroIfn` skip from a nil allocation-failure target; verify the version-specific reader resolves those targets correctly before defining their encoded representation.
5. **Continued type construction.** Preserving all context lookup associations and the original policy callback is needed if guest ixgo creates additional types. Snapshot-only invocation cannot silently be substituted for that behavior.
6. **Recursive construction completeness.** Prove the layout-compatible placeholder strategy for arrays, maps, recursive named functions, private interface identities, and any cycles created through reflectx mutation APIs. Enumerating a kind is not proof that every mutated descriptor graph of that kind is reconstructible.
7. **Capture boundary and lifetime.** Identify who stops context/type mutation during export and who owns reconstructed method targets after Load. These responsibilities cannot be enforced by a new mutex around only the adapter.

## 13. Source References

| Source | What it establishes |
|---|---|
| [reflectx/context.go][rx-context] | All Context fields, Default, NewContext, allocation policy and error shape. |
| [reflectx/rtype.go][rx-rtype] | Descriptor allocation, named types, SetUnderlying, method inspection, blank/embedded struct handling. |
| [reflectx/reflectx.go][rx-public] | Public field/type helpers, SetElem, NumMethodX and MethodByIndex. |
| [reflectx/type.go][rx-type] | Runtime descriptor aliases, user-method flag, Method input structure. |
| [reflectx/method.go][rx-method] | Method construction APIs, extracted methods, interface construction and signature cache. |
| [reflectx/methodof.go][rx-methodof] | Global caches, MakeFunc-backed methods, slot registration, T/*T construction and interface metadata. |
| [reflectx/linkname.go][rx-linkname] | Registration of type/name/text pointers with the runtime offset map. |
| [reflectx/abi/abi.go][rx-abi] | MethodInfo and the available MethodProvider contract. |
| [reflectx/internal/icall512/icall_regabi.go][rx-provider] | Register-ABI slot representation and method wrapper allocation. |
| [reflectx/x/reflect/type.go][rx-layout] | Derived call-frame layout cache and frame pools. |
| [reflectx/rtype_llgo.go][rx-llgo] | Separate LLGo descriptor, closure, and named-function behavior. |
| [ixgo/xtypes.go][ix-types] | TypesRecord caches, layout-compatible placeholders, named-type construction and method definitions. |
| [ixgo/interp.go][ix-interp] | Per-interpreter context choice, interface policy, and FindMethod callback captures. |
| [ixgo/opblock.go][ix-opblock] | Allocation closures capturing xtype.Type values. |
| [xtype/xtype.go][xtype] | `Type unsafe.Pointer` and extraction of a reflect.Type descriptor pointer. |
| [Current reflecttype](../../internal/reflecttype/reflecttype.go) | Existing Snapshot/Export/Resolve and unsupported dynamic named types. |
| [Current state encoder](../../internal/state/encode.go) | Reflected type discovery, SliceOf root exposure, and type-table finalization. |
| [Current state decoder](../../internal/state/decode.go) | Interface assignment and callback completion order. |
| [Current native function support](../../internal/state/native.go) | MakeFunc callback extraction and deferred reconstruction. |

[rx-context]: https://github.com/goplus/reflectx/blob/v1.7.8/context.go
[rx-rtype]: https://github.com/goplus/reflectx/blob/v1.7.8/rtype.go
[rx-public]: https://github.com/goplus/reflectx/blob/v1.7.8/reflectx.go
[rx-type]: https://github.com/goplus/reflectx/blob/v1.7.8/type.go
[rx-method]: https://github.com/goplus/reflectx/blob/v1.7.8/method.go
[rx-methodof]: https://github.com/goplus/reflectx/blob/v1.7.8/methodof.go
[rx-linkname]: https://github.com/goplus/reflectx/blob/v1.7.8/linkname.go
[rx-abi]: https://github.com/goplus/reflectx/blob/v1.7.8/abi/abi.go
[rx-provider]: https://github.com/goplus/reflectx/blob/v1.7.8/internal/icall512/icall_regabi.go
[rx-layout]: https://github.com/goplus/reflectx/blob/v1.7.8/x/reflect/type.go
[rx-llgo]: https://github.com/goplus/reflectx/blob/v1.7.8/rtype_llgo.go
[ix-types]: https://github.com/goplus/ixgo/blob/v1.1.6/xtypes.go
[ix-interp]: https://github.com/goplus/ixgo/blob/v1.1.6/interp.go
[ix-opblock]: https://github.com/goplus/ixgo/blob/v1.1.6/opblock.go
[xtype]: https://github.com/visualfc/xtype/blob/v0.3.3/xtype.go
