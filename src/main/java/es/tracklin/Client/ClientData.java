package es.tracklin.Client;

public class ClientData {
    public class ContactDetails {
        private String email;
        private String name;
        private String number;

        public String getEmail() {
            return email;
        }

        public void setEmail(String email) {
            this.email = email;
        }

        public String getName() {
            return name;
        }

        public void setName(String name) {
            this.name = name;
        }

        public String getNumber() {
            return number;
        }

        public void setNumber(String number) {
            this.number = number;
        }
    }

    public class Tokens {
        private String API;
        private String Interface;

        public String getAPI() {
            return API;
        }

        public void setAPI(String API) {
            this.API = API;
        }

        public String getInterface() {
            return Interface;
        }

        public void setInterface(String anInterface) {
            Interface = anInterface;
        }
    }

    public class Credentials {
        private String username;
        private String password;

        public String getUsername() {
            return username;
        }

        public void setUsername(String username) {
            this.username = username;
        }

        public String getPassword() {
            return password;
        }

        public void setPassword(String password) {
            this.password = password;
        }
    }

    private String id = "";

    private Tokens tokens;
    private ContactDetails contactDetails;
    private Credentials credentials;

    public String getId() {
        return id;
    }

    public void setId(String id) {
        this.id = id;
    }

    public Tokens getTokens() {
        return tokens;
    }

    public void setTokens(Tokens tokens) {
        this.tokens = tokens;
    }

    public ContactDetails getContactDetails() {
        return contactDetails;
    }

    public void setContactDetails(ContactDetails contactDetails) {
        this.contactDetails = contactDetails;
    }

    public Credentials getCredentials() {
        return credentials;
    }

    public void setCredentials(Credentials credentials) {
        this.credentials = credentials;
    }
}
