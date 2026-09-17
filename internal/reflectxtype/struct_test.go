package reflectxtype

import (
	"encoding/binary"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/goplus/reflectx"
)

func TestStructTypeIsolation(t *testing.T) {
	if os.Getenv("SANDBOX_REFLECTX_STRUCT_ISOLATION") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestStructTypeIsolation$", "-test.v")
		command.Env = append(os.Environ(), "SANDBOX_REFLECTX_STRUCT_ISOLATION=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("struct isolation: %v\n%s", err, output)
		}
		return
	}
	for _, embeddedFirst := range []bool{false, true} {
		name := "ordinary-first"
		if embeddedFirst {
			name = "embedded-first"
		}
		t.Run(name, func(t *testing.T) {
			fields := []reflect.StructField{{Name: "F", Type: reflect.TypeFor[int](), Tag: reflect.StructTag(name), Anonymous: true}}
			embedded := reflectx.NewContext().StructOf(fields)
			fields[0].Anonymous = false
			ordinary := reflect.StructOf(fields)
			if ordinary == embedded || ordinary.Field(0).Anonymous {
				t.Fatal("fixture did not create separate ordinary and embedded types")
			}
			owner := reflectx.NamedTypeOf("example/isolation", "Owner", ordinary)
			nested := reflect.StructOf([]reflect.StructField{{Name: "Child", Type: ordinary}})
			roots := []reflect.Type{ordinary, owner, nested, reflect.PointerTo(ordinary), embedded, reflect.PointerTo(embedded)}
			if embeddedFirst {
				roots[0], roots[4] = roots[4], roots[0]
			}
			e := exporter{ids: make(map[reflect.Type]uint32)}
			for _, typ := range roots {
				if _, err := e.intern(typ); err != nil {
					t.Fatal(err)
				}
			}
			data := binary.AppendUvarint(nil, uint64(len(e.entries)))
			for _, entry := range e.entries {
				data = binary.AppendUvarint(data, uint64(len(entry)))
				data = append(data, entry...)
			}
			sent := &Snapshot{Data: data, IDs: e.ids}
			guest, err := Open(data)
			if err != nil {
				t.Fatal(err)
			}
			plain, _ := guest.Resolve(sent.IDs[ordinary])
			embed, _ := guest.Resolve(sent.IDs[embedded])
			named, _ := guest.Resolve(sent.IDs[owner])
			child, _ := guest.Resolve(sent.IDs[nested])
			if plain == embed || plain.Field(0).Anonymous || !embed.Field(0).Anonymous || named.Field(0).Anonymous {
				t.Fatal("restoring the embedded type changed an ordinary field or merged type IDs")
			}
			if ordinary.Field(0).Anonymous || owner.Field(0).Anonymous || child.Field(0).Type != plain {
				t.Fatal("restoration changed a cached type or its dependent field")
			}
			for _, typ := range []reflect.Type{ordinary, embedded} {
				pointer, _ := guest.Resolve(sent.IDs[reflect.PointerTo(typ)])
				element, _ := guest.Resolve(sent.IDs[typ])
				if pointer.Elem() != element {
					t.Fatal("pointer lost its element identity")
				}
			}
			returned, err := guest.Export()
			if err != nil {
				t.Fatal(err)
			}
			host, err := sent.Open(returned.Data)
			if err != nil {
				t.Fatal(err)
			}
			for typ, id := range sent.IDs {
				local, _ := guest.Resolve(id)
				original, _ := host.Resolve(id)
				if returned.IDs[local] != id || original != typ {
					t.Fatalf("type %v lost its ID or host identity", typ)
				}
			}
		})
	}
}
