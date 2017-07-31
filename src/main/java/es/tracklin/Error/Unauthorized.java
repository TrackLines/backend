package es.tracklin.Error;

public class Unauthorized {
    private final String message;

    public Unauthorized() {
        message = "Unauthorized";
    }

    public String getMessage() {
        return message;
    }
}
