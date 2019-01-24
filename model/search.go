package model

import (
	"bytes"
	"context"
	"fmt"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/search"
	"reflect"
	"strings"
	"sync"
)

//flag fields that we want to search with "prototype:search"
const tagSearch string = "search"

type searchType int

const (
	// string
	_str searchType = iota
	_int
	_f64
	_html
	_time
	_key
	_geopoint
)

// describes the searchable fields for each modelable
type fieldDescriptor struct {
	index int
	name  string
	searchType
}

var searchMutex sync.Mutex
var searchableDefs = map[reflect.Type][]*fieldDescriptor{}

type searchable struct {
	*Model
}

type searchOp string

const (
	SearchNoOp searchOp = ""
	SearchAnd  searchOp = "AND"
	SearchOr   searchOp = "OR"
)

// maps the searchable fields of a given struct to searchable fields to ease the runtime retrieval
func getSearchablefields(t reflect.Type) []*fieldDescriptor {
	// we already parsed the searchable fields of type t
	searchMutex.Lock()
	if descs, ok := searchableDefs[t]; ok {
		searchMutex.Unlock()
		return descs
	}
	searchMutex.Unlock()

	var descriptors []*fieldDescriptor

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		name := field.Tag.Get(tagDomain)
		if i := strings.Index(name, ","); i != -1 {
			name = name[:i]
		}

		// the field has been flagged if it has model:search tag
		if name == tagSearch {
			desc := fieldDescriptor{}
			desc.index = i
			desc.name = field.Name

			switch field.Type.Kind() {
			case reflect.String:
				desc.searchType = _str
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				desc.searchType = _int
			case reflect.Float32, reflect.Float64:
				desc.searchType = _f64
			case reflect.Struct:
				switch field.Type {
				case typeOfTime:
					desc.searchType = _time
				case typeOfGeoPoint:
					desc.searchType = _geopoint
				default:
					if reflect.PtrTo(field.Type).Implements(typeOfModelable) {
						desc.searchType = _key
					}
				}
			}

			descriptors = append(descriptors, &desc)
		}
	}
	searchMutex.Lock()
	searchableDefs[t] = descriptors
	searchMutex.Unlock()

	return descriptors
}

func (model *searchable) Save() ([]search.Field, *search.DocumentMetadata, error) {

	descs := getSearchablefields(reflect.TypeOf(model.modelable).Elem())
	l := len(descs)

	if l == 0 {
		return nil, nil, nil
	}

	val := reflect.ValueOf(model.modelable).Elem()

	fields := make([]search.Field, l, cap(descs))

	for i, desc := range descs {
		sf := &fields[i]
		sf.Name = desc.name

		field := val.Field(desc.index)
		switch desc.searchType {
		case _str, _html:
			sf.Value = field.String()
		case _f64:
			sf.Value = float64(field.Float())
		case _int:
			sf.Value = float64(field.Int())
		case _time, _geopoint:
			sf.Value = field.Elem()
		case _key:
			key := model.references[desc.index].Key
			sf.Value = search.Atom(key.Encode())
		}
	}

	return fields, nil, nil

}

// adds the model to the index
func searchPut(ctx context.Context, model *Model, name string) error {

	index, err := search.Open(name)
	if nil != err {
		return err
	}

	_, err = index.Put(ctx, model.EncodedKey(), &searchable{Model: model})

	return err
}

func searchPutMulti(ctx context.Context, models []*Model, name string) error {
	keys := make([]string, len(models), cap(models))
	items := make([]interface{}, len(models), cap(models))
	for i := range models {
		keys[i] = models[i].EncodedKey()
		searchable := &searchable{Model: models[i]}
		items[i] = searchable
	}

	index, err := search.Open(name)

	if err != nil {
		panic(err)
		recover()
		return err
	}

	_, err = index.PutMulti(ctx, keys, items)

	return err
}

func searchDelete(ctx context.Context, model Model, name string) error {
	index, err := search.Open(name)
	if nil != err {
		return nil
	}

	return index.Delete(ctx, model.EncodedKey())
}

//stays at nil -> ignores the struct datas and gets a key only query from datastore
//which will load the struct with Read()
func (model *searchable) Load(fields []search.Field, meta *search.DocumentMetadata) error {
	return nil
}

type searchQuery struct {
	name  string
	mType reflect.Type
	query bytes.Buffer
}

func NewSearchQuery(m modelable) *searchQuery {
	t := reflect.TypeOf(m).Elem()
	name := t.Name()
	return &searchQuery{mType: t, name: name}
}

func NewSearchQueryWithName(m modelable, name string) *searchQuery {
	t := reflect.TypeOf(m).Elem()
	return &searchQuery{mType: t, name: name}
}

func (sq *searchQuery) SearchWith(query string) {
	sq.query.WriteString(query)
}

//so far, op is the logical operation to use with the reference, i.e. AND, OR.
//with reference is always an equality
func (sq *searchQuery) SearchWithModel(field string, ref modelable, op searchOp) {

	// we have at least one query, append the operation to it
	if sq.query.Len() != 0 && op != SearchNoOp {
		sq.query.WriteString(" ")
		sq.query.WriteString(string(op))
		sq.query.WriteString(" ")
	}

	sq.query.WriteString(field)
	sq.query.WriteString(ref.getModel().EncodedKey())
}

func (sq *searchQuery) Search(ctx context.Context, dst interface{}, opts *search.SearchOptions) (int, error) {

	dstv := reflect.ValueOf(dst)

	if !isValidContainer(dstv) {
		return 0, fmt.Errorf("invalid container of type %s. Container must be a modelable slice", dstv.Elem().Type().Name())
	}

	modelables := dstv.Elem()

	//always do a id-only key. retrieval is demanded to model
	if nil == opts {
		opts = &search.SearchOptions{}
	}
	opts.IDsOnly = true

	idx, err := search.Open(sq.name)

	if err != nil {
		panic(err)
	}

	query := sq.query.String()

	count := 0

	for it := idx.Search(ctx, query, opts); ; {
		count = it.Count()
		k, e := it.Next(nil)

		if e == search.Done {
			break
		}

		newModelable := reflect.New(sq.mType)
		m, ok := newModelable.Interface().(modelable)

		if !ok {
			err = fmt.Errorf("can't cast struct of type %s to modelable", sq.mType.Name())
			sq = nil
			return count, err
		}

		//Note: indexing here assigns the address of m to the Model.
		//this means that if a user supplied a populated dst we must reindex its elements before returning
		//or the model will point to a different modelable
		index(m)

		model := m.getModel()
		model.Key, err = datastore.DecodeKey(k)
		if err != nil {
			// todo: handle case
			return count, err
		}

		modelables.Set(reflect.Append(modelables, reflect.ValueOf(m)))
	}

	return count, ReadMulti(ctx, reflect.Indirect(dstv).Interface())

}
