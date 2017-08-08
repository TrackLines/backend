package es.tracklin.DataLayer.Hibernate;

import es.tracklin.Backend;
import es.tracklin.Client.ClientData;
import es.tracklin.Client.Creation;
import es.tracklin.DataConnections.Hibernate;

import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.test.context.junit4.SpringRunner;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class)
public class DataTests {
    @Autowired
    es.tracklin.Configuration.Hibernate hibernateConfiguration;

    @Autowired
    es.tracklin.Configuration.Tokens tokensConfiguration;

    private String username = "bob";
    private String password = "bob";
    private String email = "bob@bob.bob";

    @Test
    public void shouldAddUser() throws Exception {
        Hibernate hibernate = new Hibernate();
        hibernate.setHibernateConfiguration(hibernateConfiguration);

        ClientData clientData = new ClientData();
        ClientData.Credentials credentials = clientData.new Credentials();
        ClientData.ContactDetails contactDetails = clientData.new ContactDetails();

        credentials.setPassword(password);
        credentials.setUsername(username);
        clientData.setCredentials(credentials);

        Creation creation = new Creation();
        creation.setTokens(tokensConfiguration);
        String web = creation.web(username);
        String api = creation.api(username);

        //hibernate.addUser(clientData);
    }

    @Test
    public void shouldUpdateUser() throws Exception {

    }

    @Test
    public void shouldDeleteUser() throws Exception {

    }
}
