package es.tracklin.Tracks;

public class Track {
    private final long id;
    private final String content;

    public Track(long id, String content) {
        this.id = id;
        this.content = content;
    }

    public long getId() {
        return id;
    }

    public String getContent() {
        return content;
    }
}
