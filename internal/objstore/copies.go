package objstore

// CopyConcurrency bounds how many parts a PutParts copies on the store's
// side at once (T82-10: an upload in parts is completed by copying each of
// its staged 5 MiB parts). A copy moves no bytes through the caller; each
// is one request the store answers when it has copied the range, so their
// time adds up one after another unless they overlap. 8 at once: an
// upload's complete of 2,000 parts at the large-Upstream gate's 100 ms a
// request takes about 25 s of copy requests rather than 200 s; and the
// four uploads one caller may have in flight complete with at most 32
// copies at once, the store client's idle connections per host
// (storeguard's MaxIdleConnsPerHost), so connections are reused rather
// than made and dropped.
const CopyConcurrency = 8
