package es.tracklin.Mongo;

import es.tracklin.Backend;
import es.tracklin.Client.ClientData;
import es.tracklin.Configuration.MongoConfiguration;
import es.tracklin.DataConnections.Mongo;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.test.context.junit4.SpringRunner;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotEquals;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class)
public class DataTests {
    @Autowired
    MongoConfiguration mongoConfiguration;

    @Test
    public void shouldInsertData() throws Exception {
        Mongo mongo = new Mongo(mongoConfiguration);

        ClientData clientData = new ClientData();
        ClientData.Tokens tokens = clientData.new Tokens();
        ClientData.ContactDetails contactDetails = clientData.new ContactDetails();

        clientData.setName("bob");
        clientData.setPassword("bob");

        contactDetails.setEmail("bob@bob.bob");
        contactDetails.setName("bobster");
        contactDetails.setNumber("00");
        clientData.setContactDetails(contactDetails);

        tokens.setAPI("bob");
        tokens.setInterface("bob");
        clientData.setTokens(tokens);
        assertNotEquals("", mongo.addUser(clientData));
    }

    @Test
    public void shouldGetData() throws Exception {
        Mongo mongo = new Mongo(mongoConfiguration);

        // Insert the data to be returned
        ClientData insertData = new ClientData();
        ClientData.Tokens tokens = insertData.new Tokens();
        ClientData.ContactDetails contactDetails = insertData.new ContactDetails();

        String username = "bob";
        String password = "bob";

        insertData.setName(username);
        insertData.setPassword(password);

        contactDetails.setEmail("bob@bob.bob");
        contactDetails.setName("bobster");
        contactDetails.setNumber("00");
        insertData.setContactDetails(contactDetails);

        tokens.setAPI("bob");
        tokens.setInterface("bob");
        insertData.setTokens(tokens);

        // Get the insert id
        String userId = mongo.addUser(insertData);

        // Retrieve data
        ClientData returnData = mongo.getUser(username, password);
        assertEquals(userId, returnData.getId());
    }

    @Test
    public void shouldDeleteUser() throws Exception {
        Mongo mongo = new Mongo(mongoConfiguration);

        String username = "bob";
        String password = "bob";

        // Delete the user
        mongo.deleteUser(username, password);

        // Should return non user
        ClientData returnData = mongo.getUser(username, password);
        assertEquals("", returnData.getId());
    }
}
