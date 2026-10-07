package shapes

import "reflect"

// CallByName dispatches by reflection.
func CallByName(v any, name string) {
	reflect.ValueOf(v).MethodByName(name).Call(nil)
}
